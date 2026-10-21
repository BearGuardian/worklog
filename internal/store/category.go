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
// 16 色 = 8 个色系 × 深/浅两档。同一个色系的两档 L* 差 15 以上,摆在一起
// 一眼能分出深浅;深浅两档之间还夹着别的色系,所以同色系的不会挨着出现。
//
// **顺序是刻意交错的,别按色系归拢。** 调色板是按这个顺序取用的,而分类列表
// 按 sort_order(≈创建顺序)显示 —— 要是把深浅成对排在一起,相邻两个分类
// 就会拿到同色系的深浅两档,那反而更像。交错之后前 8 个分类拿到 8 个铺开的
// 色相(实测相邻取色 ΔE 最小 39.8),第 9 个开始才回到同色系的另一档。
//
// 挑的是中等明度,深浅两套主题下都看得清。改这里要跑 TestPaletteDistinct ——
// 它会检查任意两色的 ΔE ≥ 15,防止以后往里面塞近似色。
var palette = []string{
	"#d0393f", "#3fc0d4", "#a37c0e", "#a89bff",
	"#178a55", "#ef7ab8", "#3566d8", "#f59b52",
	"#f2747c", "#0b7c8d", "#d9b73f", "#7c6bff",
	"#4fc48d", "#c9418f", "#6ea6f5", "#cf6415",
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

// nextColor 给新分类挑一个颜色。
func (s *Store) nextColor() (string, error) { return s.nextColorFor("categories") }

// nextColorFor 给新记录挑一个颜色:优先挑没被用过的,都用过了挑用得最少的。
//
// 统计走 normHex 归一化 —— 用户在原生取色器里挑出 #7C6BFF 时不能被当成
// 一个"还没用过"的颜色。
//
// table 是内部字面量,不是用户输入,所以直接拼进语句没有注入面。
// 分类和组件各查各的表:它们显示在两个互斥的视图里,跨表撞色看不出来。
func (s *Store) nextColorFor(table string) (string, error) {
	rows, err := s.db.Query(`SELECT color FROM ` + table)
	if err != nil {
		return "", fmt.Errorf("读取已有颜色失败: %w", err)
	}
	defer rows.Close()

	used := map[string]int{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return "", fmt.Errorf("读取颜色失败: %w", err)
		}
		used[normHex(c)]++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return pickUnused(used), nil
}
