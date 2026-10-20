package sqlite

import (
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/share"
)

// Peers 实现 ports.PeerDirectory。
type Peers struct {
	db  *DB
	clk ports.Clock
}

var _ ports.PeerDirectory = (*Peers)(nil)

// NewPeers 构造节点目录仓储。
func NewPeers(db *DB, clk ports.Clock) *Peers { return &Peers{db: db, clk: clk} }

const peerColumns = `node_id, display_name, pub_key, last_addr, caps, proto_ver,
                     first_seen, last_seen, subnet, state, source, shares, app_version`

// Upsert 写入/更新节点条目。
//
// 空字段（"" / 0）不覆盖已有值：announce 与目录条目携带的信息量不同，
// 不能让信息更少的来源把信息抹掉。
func (p *Peers) Upsert(in peer.Peer) error {
	return p.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
INSERT INTO peers(node_id, display_name, pub_key, last_addr, caps, proto_ver,
                  first_seen, last_seen, subnet, state, source, shares, app_version)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(node_id) DO UPDATE SET
  display_name = CASE WHEN excluded.display_name <> '' THEN excluded.display_name ELSE peers.display_name END,
  pub_key      = CASE WHEN excluded.pub_key      <> '' THEN excluded.pub_key      ELSE peers.pub_key      END,
  last_addr    = CASE WHEN excluded.last_addr    <> '' THEN excluded.last_addr    ELSE peers.last_addr    END,
  caps         = CASE WHEN excluded.caps         <> 0  THEN excluded.caps         ELSE peers.caps         END,
  proto_ver    = CASE WHEN excluded.proto_ver    <> 0  THEN excluded.proto_ver    ELSE peers.proto_ver    END,
  subnet       = CASE WHEN excluded.subnet       <> '' THEN excluded.subnet       ELSE peers.subnet       END,
  first_seen   = CASE WHEN peers.first_seen IS NULL OR peers.first_seen = 0
                      THEN excluded.first_seen ELSE peers.first_seen END,
  last_seen    = excluded.last_seen,
  -- blocked 是粘性状态：任何写入都不得覆盖它，否则每新增一条写入路径（announce /
  -- 连接事件 / 目录交换）都是一次「被移除的节点复活」的风险。
  -- 解除只能走 SetSuppressed（见 ports.PeerDirectory）。
  state        = CASE WHEN peers.state = 'blocked' THEN peers.state ELSE excluded.state END,
  source       = excluded.source,
  -- shares 【刻意不沿用】上面「空值不覆盖」的规则：摘要是对方周期性通告的
  -- 全量状态，对方取消发布后正文就是空的。保留旧值会让取消掉的共享一直
  -- 挂在别人列表里，直到 TTL 过期 —— 用户看到的是「点了提示共享不存在」。
  shares       = excluded.shares,
  -- 版本与 shares 相反，沿用「空值不覆盖」：空串只表示「这条通告没说」
  -- （老版本客户端），而版本不会随时间被"取消"。
  app_version  = CASE WHEN excluded.app_version <> '' THEN excluded.app_version ELSE peers.app_version END`,
			in.NodeID.String(), in.DisplayName, pubKeyString(in.PublicKey), in.LastAddr,
			uint32(in.Caps), in.ProtoVer,
			msOf(in.FirstSeen), msOf(in.LastSeen), in.Subnet, string(in.State), in.Source,
			encodeShares(in.Shares), in.AppVersion,
		)
		return err
	})
}

// Get 读取节点条目。
func (p *Peers) Get(id identity.NodeID) (peer.Peer, bool) {
	row := p.db.QueryRow(`SELECT `+peerColumns+` FROM peers WHERE node_id = ?`, id.String())
	out, err := scanPeer(row)
	if err != nil {
		return peer.Peer{}, false
	}
	return out, true
}

// List 按 last_seen 倒序返回条目。
func (p *Peers) List(filter ports.PeerFilter) []peer.Peer {
	q := `SELECT ` + peerColumns + ` FROM peers`
	var (
		where []string
		args  []any
	)
	if filter.State != "" {
		where = append(where, "state = ?")
		args = append(args, string(filter.State))
	} else if !filter.IncludeBlocked {
		// 默认不返回「被本地移除」的条目（见 ports.PeerFilter.IncludeBlocked）。
		// 显式按 State 查询优先：诊断面板要能看到那几条。
		// 这里不需要 COALESCE：state 恒由 Upsert 写入，列也有 DEFAULT 'unknown'，
		// 不会出现 NULL（NULL 会让 `state <> 'blocked'` 求值为 NULL 而丢掉整行）。
		where = append(where, "state <> 'blocked'")
	}
	if filter.Source != "" {
		where = append(where, "source = ?")
		args = append(args, filter.Source)
	}
	if filter.OnlineOnly {
		where = append(where, "state IN ('online','discovered')")
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY last_seen DESC"
	if filter.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, filter.Limit)
	}

	rows, err := p.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []peer.Peer
	for rows.Next() {
		item, err := scanPeer(rows)
		if err != nil {
			return out
		}
		out = append(out, item)
	}
	return out
}

// MarkOffline 标记节点离线（保留条目）。
func (p *Peers) MarkOffline(id identity.NodeID, at time.Time) error {
	return p.db.Write(func(tx *sql.Tx) error {
		// 已移除的条目不能被离线事件变回可见：List 只排除 blocked，而 offline 是可见的。
		_, err := tx.Exec(
			`UPDATE peers SET state = 'offline', last_seen = ?
			  WHERE node_id = ? AND state <> 'blocked'`,
			msOf(at), id.String(),
		)
		return err
	})
}

// ResetLiveness 清零上次运行残留的在线状态（语义见 ports.PeerDirectory）。
//
// 单条 UPDATE 而不是先 List 再逐条写：启动清理必须是一次原子操作，
// 中途失败也不该留下「清了一半」的目录。last_seen 保持原值（见端口契约）。
func (p *Peers) ResetLiveness() (int, error) {
	var affected int64
	err := p.db.Write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE peers SET state = 'discovered' WHERE state = 'online'`)
		if err != nil {
			return err
		}
		affected, _ = res.RowsAffected()
		return nil
	})
	return int(affected), err
}

// SetSuppressed 设置/清除「本地已移除」标记：复用 state='blocked'（设计文档 §5.2）。
//
// 只更新已存在的行：删除的前提是「它出现在我的列表里」，没有行时什么都不做（幂等）。
// at 记进 last_seen，让「什么时候删的」在诊断里可见。
func (p *Peers) SetSuppressed(id identity.NodeID, suppressed bool, at time.Time) error {
	return p.db.Write(func(tx *sql.Tx) error {
		if suppressed {
			_, err := tx.Exec(
				`UPDATE peers SET state = 'blocked', last_seen = ? WHERE node_id = ?`,
				msOf(at), id.String(),
			)
			return err
		}
		// 解除只对「当前确实被移除」的行生效：无条件写 discovered 会把一个在线节点降级，
		// 与 mem 实现（同样只动 blocked）出现行为分歧，而两者是互为替身的关系。
		_, err := tx.Exec(
			`UPDATE peers SET state = 'discovered', last_seen = ?
			  WHERE node_id = ? AND state = 'blocked'`,
			msOf(at), id.String(),
		)
		return err
	})
}

// SeenRecently 判断该节点是否在 window 内被再次见到。
func (p *Peers) SeenRecently(id identity.NodeID, window time.Duration) bool {
	row := p.db.QueryRow(`SELECT last_seen FROM peers WHERE node_id = ?`, id.String())
	var last sql.NullInt64
	if err := row.Scan(&last); err != nil {
		return false
	}
	if !last.Valid || last.Int64 == 0 {
		return false
	}
	return p.clk.Now().Sub(time.UnixMilli(last.Int64)) < window
}

func scanPeer(s rowScanner) (peer.Peer, error) {
	var (
		out       peer.Peer
		nodeID    string
		dispName  sql.NullString
		pubKey    sql.NullString
		lastAddr  sql.NullString
		caps      sql.NullInt64
		protoVer  sql.NullInt64
		firstSeen sql.NullInt64
		lastSeen  sql.NullInt64
		subnet    sql.NullString
		state     sql.NullString
		source    sql.NullString
		shares    sql.NullString
		appVer    sql.NullString
	)
	if err := s.Scan(&nodeID, &dispName, &pubKey, &lastAddr, &caps, &protoVer,
		&firstSeen, &lastSeen, &subnet, &state, &source, &shares, &appVer); err != nil {
		return out, err
	}

	id, err := identity.ParseNodeID(nodeID)
	if err != nil {
		return out, err
	}
	out.NodeID = id
	out.DisplayName = nullString(dispName)
	out.PublicKey = parsePubKey(nullString(pubKey))
	out.LastAddr = nullString(lastAddr)
	out.Caps = peer.Caps(uint32(nullInt64(caps)))
	out.ProtoVer = int(nullInt64(protoVer))
	out.FirstSeen = timeOf(nullInt64(firstSeen))
	out.LastSeen = timeOf(nullInt64(lastSeen))
	out.Subnet = nullString(subnet)
	out.State = peer.State(nullString(state))
	out.Source = nullString(source)
	out.Shares = decodeShares(nullString(shares))
	out.AppVersion = nullString(appVer)
	return out, nil
}

// encodeShares 把共享摘要折成一行 JSON。
//
// 解析失败的一律当作空：摘要是可丢的展示数据，为了它让整条节点记录读不出来
// （进而让该节点从列表里消失）是完全不成比例的代价。
func encodeShares(in []share.Summary) string {
	in = share.NormalizeSummaries(in)
	if len(in) == 0 {
		return ""
	}
	b, err := json.Marshal(in)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeShares 还原共享摘要。
func decodeShares(s string) []share.Summary {
	if s == "" {
		return nil
	}
	var out []share.Summary
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return share.NormalizeSummaries(out)
}

func pubKeyString(pub ed25519.PublicKey) string {
	if len(pub) != ed25519.PublicKeySize {
		return ""
	}
	return identity.MarshalPublic(pub)
}

func parsePubKey(s string) ed25519.PublicKey {
	if s == "" {
		return nil
	}
	pub, err := identity.ParsePublic(s)
	if err != nil {
		return nil
	}
	return pub
}
