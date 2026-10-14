package sqlite

import (
	"database/sql"
	"strings"

	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/share"
)

// Shares 实现 ports.ShareRepo：本机发布的共享 + 其文件清单。
type Shares struct {
	db *DB
}

var _ ports.ShareRepo = (*Shares)(nil)

// NewShares 构造共享仓储。
func NewShares(db *DB) *Shares { return &Shares{db: db} }

// shareColumns 是 shares 表的列清单。
//
// 与 transfers.go 同一约定：列清单 / INSERT 列 / 实参 / Scan 目标四处必须
// 严格同序，新增列一律加在【尾部】。
const shareColumns = `share_id, name, owner_id, owner_name, root_path, is_dir,
                      vis_type, vis_allowed, total_size, item_count, created_at, updated_at`

// Upsert 整份写入一条共享（含文件清单）。
//
// 清单是【整份替换】而不是增量合并：一次「刷新索引」必须能删掉源目录里
// 已经不存在的文件，而增量合并做不到这件事（它只会越并越多）。
// 两步在同一事务里，中途失败不会留下「新元数据 + 旧清单」的半截状态。
func (s *Shares) Upsert(in share.Share) error {
	vis := in.Visibility.Normalize()
	return s.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
INSERT INTO shares(share_id, name, owner_id, owner_name, root_path, is_dir,
                   vis_type, vis_allowed, total_size, item_count, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(share_id) DO UPDATE SET
  name        = excluded.name,
  owner_id    = excluded.owner_id,
  owner_name  = excluded.owner_name,
  root_path   = excluded.root_path,
  is_dir      = excluded.is_dir,
  vis_type    = excluded.vis_type,
  vis_allowed = excluded.vis_allowed,
  total_size  = excluded.total_size,
  item_count  = excluded.item_count,
  -- created_at 不被覆盖：它是「这条共享什么时候建的」，重刷索引不该改它。
  created_at  = shares.created_at,
  updated_at  = excluded.updated_at`,
			in.ID, in.Name, in.Owner, in.OwnerName, in.RootPath, boolInt(in.IsDir),
			string(vis.Type), strings.Join(vis.Allowed, ","), in.TotalSize, len(in.Items),
			msOf(in.CreatedAt), msOf(in.UpdatedAt),
		)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(`DELETE FROM share_items WHERE share_id = ?`, in.ID); err != nil {
			return err
		}
		if len(in.Items) == 0 {
			return nil
		}
		stmt, err := tx.Prepare(`INSERT INTO share_items(share_id, path, name, size, hash, mtime, is_dir)
                                 VALUES(?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, it := range in.Items {
			if _, err := stmt.Exec(
				in.ID, it.Path, it.Name, it.Size, it.Hash, it.MTime, boolInt(it.IsDir)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Get 读取一条共享（含清单）。
func (s *Shares) Get(shareID string) (share.Share, bool) {
	row := s.db.QueryRow(`SELECT `+shareColumns+` FROM shares WHERE share_id = ?`, shareID)
	out, err := scanShare(row)
	if err != nil {
		return share.Share{}, false
	}
	items, err := s.itemsOf(shareID)
	if err != nil {
		return share.Share{}, false
	}
	out.Items = items
	return out, true
}

// List 按创建时间升序返回全部本地共享（清单一并读出）。
//
// 一次把清单读出来而不是让调用方逐条再查：共享的总量是个位数，
// 而 N+1 次查询在 SQLite 上同样要 N 次锁获取 —— 这点开销换两个实现的
// 语义完全一致（mem 侧就是一次 map 遍历）是划算的。
func (s *Shares) List() []share.Share {
	rows, err := s.db.Query(`SELECT ` + shareColumns + ` FROM shares ORDER BY created_at, share_id`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []share.Share
	for rows.Next() {
		item, err := scanShare(rows)
		if err != nil {
			return out
		}
		out = append(out, item)
	}
	for i := range out {
		items, err := s.itemsOf(out[i].ID)
		if err != nil {
			return out
		}
		out[i].Items = items
	}
	return out
}

// Delete 删除一条共享及其清单（幂等）。
func (s *Shares) Delete(shareID string) error {
	return s.db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM share_items WHERE share_id = ?`, shareID); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM shares WHERE share_id = ?`, shareID)
		return err
	})
}

// itemsOf 读取一条共享的清单，按路径升序（目录树顺序）。
func (s *Shares) itemsOf(shareID string) ([]share.Item, error) {
	rows, err := s.db.Query(
		`SELECT path, name, size, hash, mtime, is_dir FROM share_items WHERE share_id = ? ORDER BY path`,
		shareID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []share.Item
	for rows.Next() {
		var (
			it    share.Item
			size  sql.NullInt64
			hash  sql.NullString
			mtime sql.NullInt64
			isDir sql.NullInt64
		)
		if err := rows.Scan(&it.Path, &it.Name, &size, &hash, &mtime, &isDir); err != nil {
			return nil, err
		}
		it.Size = nullInt64(size)
		it.Hash = nullString(hash)
		it.MTime = nullInt64(mtime)
		it.IsDir = nullInt64(isDir) != 0
		out = append(out, it)
	}
	return out, rows.Err()
}

// scanShare 读一行 shares。
func scanShare(s rowScanner) (share.Share, error) {
	var (
		out        share.Share
		ownerName  sql.NullString
		rootPath   sql.NullString
		isDir      sql.NullInt64
		visType    sql.NullString
		visAllowed sql.NullString
		totalSize  sql.NullInt64
		createdAt  sql.NullInt64
		updatedAt  sql.NullInt64
	)
	if err := s.Scan(
		&out.ID, &out.Name, &out.Owner, &ownerName, &rootPath, &isDir,
		&visType, &visAllowed, &totalSize, new(sql.NullInt64),
		&createdAt, &updatedAt,
	); err != nil {
		return out, err
	}
	out.OwnerName = nullString(ownerName)
	out.RootPath = nullString(rootPath)
	out.IsDir = nullInt64(isDir) != 0
	out.Visibility = share.Visibility{
		Type:    share.VisibilityType(nullString(visType)),
		Allowed: splitAllowed(nullString(visAllowed)),
	}.Normalize()
	out.TotalSize = nullInt64(totalSize)
	out.CreatedAt = timeOf(nullInt64(createdAt))
	out.UpdatedAt = timeOf(nullInt64(updatedAt))
	return out, nil
}

// splitAllowed 解析逗号分隔的白名单。
func splitAllowed(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// boolInt 把布尔折成 SQLite 的 INTEGER 0/1。
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
