package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"worklog/internal/model"
)

// palette 是新分类的取色顺序。
//
// 挑的是中等饱和度,深浅两套主题下都看得清。取色时优先挑还没被用过的,
// 用完一轮再从头来 —— 分类不多时不会出现两个同色。
var palette = []string{
	"#7c6bff", "#2fb98a", "#f0913a", "#3b8fe8",
	"#e8558a", "#0ea5b7", "#c9a227", "#8b5cf6",
}

// ErrCategoryExists 表示同名分类已存在。
var ErrCategoryExists = errors.New("分类已存在")

// ListCategories 按 sort_order 返回全部分类。
func (s *Store) ListCategories() ([]model.Category, error) {
	rows, err := s.db.Query(
		`SELECT id, name, color, sort_order, created_at FROM categories
		 ORDER BY sort_order, name`)
	if err != nil {
		return nil, fmt.Errorf("查询分类失败: %w", err)
	}
	defer rows.Close()

	out := []model.Category{}
	for rows.Next() {
		var c model.Category
		var created int64
		if err := rows.Scan(&c.ID, &c.Name, &c.Color, &c.SortOrder, &created); err != nil {
			return nil, fmt.Errorf("读取分类失败: %w", err)
		}
		c.CreatedAt = fromUnix(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCategory 按 ID 取分类。不存在时返回 (nil, nil)。
func (s *Store) GetCategory(id string) (*model.Category, error) {
	var c model.Category
	var created int64
	err := s.db.QueryRow(
		`SELECT id, name, color, sort_order, created_at FROM categories WHERE id = ?`, id,
	).Scan(&c.ID, &c.Name, &c.Color, &c.SortOrder, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询分类失败: %w", err)
	}
	c.CreatedAt = fromUnix(created)
	return &c, nil
}

// CategoryByName 按名字取分类(忽略首尾空白)。不存在时返回 (nil, nil)。
func (s *Store) CategoryByName(name string) (*model.Category, error) {
	var c model.Category
	var created int64
	err := s.db.QueryRow(
		`SELECT id, name, color, sort_order, created_at FROM categories WHERE name = ?`,
		strings.TrimSpace(name),
	).Scan(&c.ID, &c.Name, &c.Color, &c.SortOrder, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询分类失败: %w", err)
	}
	c.CreatedAt = fromUnix(created)
	return &c, nil
}

// CreateCategory 新建分类。color 为空时自动挑一个还没被用过的颜色。
func (s *Store) CreateCategory(name, color string) (*model.Category, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("分类名不能为空")
	}

	if exist, err := s.CategoryByName(name); err != nil {
		return nil, err
	} else if exist != nil {
		return nil, fmt.Errorf("%w: %s", ErrCategoryExists, name)
	}

	if color == "" {
		c, err := s.nextColor()
		if err != nil {
			return nil, err
		}
		color = c
	}

	var maxOrder int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(sort_order), -1) FROM categories`).Scan(&maxOrder); err != nil {
		return nil, fmt.Errorf("读取分类顺序失败: %w", err)
	}

	c := model.Category{ID: newID(), Name: name, Color: color, SortOrder: maxOrder + 1}
	if _, err := s.db.Exec(
		`INSERT INTO categories (id, name, color, sort_order, created_at) VALUES (?,?,?,?,?)`,
		c.ID, c.Name, c.Color, c.SortOrder, unix(nowFunc()),
	); err != nil {
		return nil, fmt.Errorf("新建分类失败: %w", err)
	}
	c.CreatedAt = nowFunc()
	return &c, nil
}

// EnsureCategory 按名字找分类,没有就建一个。
//
// 这是"输入不存在的分类名就自动创建"的落点 —— 记录的动作不该被
// "先去建分类"打断。
func (s *Store) EnsureCategory(name string) (*model.Category, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil // 未分类是合法状态,不是错误
	}
	if c, err := s.CategoryByName(name); err != nil {
		return nil, err
	} else if c != nil {
		return c, nil
	}
	c, err := s.CreateCategory(name, "")
	if err != nil {
		// 并发下可能刚好被别人建了,再查一次
		if errors.Is(err, ErrCategoryExists) {
			return s.CategoryByName(name)
		}
		return nil, err
	}
	return c, nil
}

// UpdateCategory 改名字或颜色。空字符串表示不改。
func (s *Store) UpdateCategory(id, name, color string) error {
	name = strings.TrimSpace(name)

	if name != "" {
		// 同名检查:UNIQUE 约束也会拦,但先查一次能给出更清楚的错误
		if other, err := s.CategoryByName(name); err != nil {
			return err
		} else if other != nil && other.ID != id {
			return fmt.Errorf("%w: %s", ErrCategoryExists, name)
		}
	}

	sets := []string{}
	args := []any{}
	if name != "" {
		sets = append(sets, "name = ?")
		args = append(args, name)
	}
	if color != "" {
		sets = append(sets, "color = ?")
		args = append(args, color)
	}
	if len(sets) == 0 {
		return nil
	}

	args = append(args, id)
	res, err := s.db.Exec(`UPDATE categories SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("更新分类失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("分类不存在: %s", id)
	}
	return nil
}

// DeleteCategory 删除分类。
//
// 事项**不会**跟着删,只是退回未分类(category_id 置空,靠外键的
// ON DELETE SET NULL)。整理分类不该弄丢记录。
func (s *Store) DeleteCategory(id string) error {
	res, err := s.db.Exec(`DELETE FROM categories WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除分类失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("分类不存在: %s", id)
	}
	return nil
}

// nextColor 挑一个还没被用过的调色板颜色;都用过了就按分类数取模。
func (s *Store) nextColor() (string, error) {
	rows, err := s.db.Query(`SELECT color FROM categories`)
	if err != nil {
		return "", fmt.Errorf("读取已有颜色失败: %w", err)
	}
	defer rows.Close()

	used := map[string]bool{}
	n := 0
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return "", fmt.Errorf("读取颜色失败: %w", err)
		}
		used[c] = true
		n++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	for _, c := range palette {
		if !used[c] {
			return c, nil
		}
	}
	return palette[n%len(palette)], nil
}
