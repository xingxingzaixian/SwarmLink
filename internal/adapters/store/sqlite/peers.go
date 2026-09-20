package sqlite

import (
	"crypto/ed25519"
	"database/sql"
	"strings"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
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
                     first_seen, last_seen, subnet, state, source`

// Upsert 写入/更新节点条目。
//
// 空字段（"" / 0）不覆盖已有值：announce 与目录条目携带的信息量不同，
// 不能让信息更少的来源把信息抹掉。
func (p *Peers) Upsert(in peer.Peer) error {
	return p.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
INSERT INTO peers(node_id, display_name, pub_key, last_addr, caps, proto_ver,
                  first_seen, last_seen, subnet, state, source)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
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
  state        = excluded.state,
  source       = excluded.source`,
			in.NodeID.String(), in.DisplayName, pubKeyString(in.PublicKey), in.LastAddr,
			uint32(in.Caps), in.ProtoVer,
			msOf(in.FirstSeen), msOf(in.LastSeen), in.Subnet, string(in.State), in.Source,
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
		_, err := tx.Exec(
			`UPDATE peers SET state = 'offline', last_seen = ? WHERE node_id = ?`,
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
	)
	if err := s.Scan(&nodeID, &dispName, &pubKey, &lastAddr, &caps, &protoVer,
		&firstSeen, &lastSeen, &subnet, &state, &source); err != nil {
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
	return out, nil
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
