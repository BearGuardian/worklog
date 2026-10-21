package store

import (
	"math"
	"testing"
)

func TestNormHex(t *testing.T) {
	cases := []struct{ in, want string }{
		{"#7C6BFF", "#7c6bff"},
		{"  #7c6bff  ", "#7c6bff"},
		{"#000000", "#000000"},
		// 解析不了的应当原样返回,交给上层处理
		{"", ""},
		{"7c6bff", "7c6bff"},
		{"#abc", "#abc"},
		{"#gggggg", "#gggggg"},
		{"rgba(0,0,0,.5)", "rgba(0,0,0,.5)"},
	}
	for _, c := range cases {
		if got := normHex(c.in); got != c.want {
			t.Errorf("normHex(%q) = %q,期望 %q", c.in, got, c.want)
		}
	}
}

func TestColorDistKnownPairs(t *testing.T) {
	// 这两个是老调色板里"看着一样"的一对:字符串不同,肉眼几乎分不出。
	// 去重阈值必须能抓住它 —— 这是这次改动的起因,单独钉住。
	if d := colorDist("#7c6bff", "#8b5cf6"); d >= colorMinDist {
		t.Errorf("老调色板那对紫的 ΔE 应当 < %.0f,实际 %.1f", colorMinDist, d)
	}

	// 同一个色,距离必须是 0
	if d := colorDist("#7c6bff", "#7C6BFF"); d != 0 {
		t.Errorf("大小写不同不该产生距离,实际 %.1f", d)
	}

	// 黑白的 ΔE 是量纲上限附近,拿来确认没算错
	if d := colorDist("#000000", "#ffffff"); math.Abs(d-100) > 1 {
		t.Errorf("黑白 ΔE 应当接近 100,实际 %.1f", d)
	}

	// 真正不同的颜色不该被误判成"一样"
	if d := colorDist("#d0393f", "#cf6415"); d <= colorMinDist {
		t.Errorf("红和橙不该被当成重复,实际 ΔE %.1f", d)
	}

	// 解析不了的颜色返回 +Inf:一个认不出来的色不该被当成和谁重复
	if d := colorDist("不是颜色", "#7c6bff"); !math.IsInf(d, 1) {
		t.Errorf("解析失败应当返回 +Inf,实际 %v", d)
	}
}

// 调色板里任意两色的 ΔE 都必须够大 —— 这是"颜色丰富"这件事的底线。
// 以后往 palette 里塞颜色,这条会拦住近似色。
func TestPaletteDistinct(t *testing.T) {
	if len(palette) != 16 {
		t.Errorf("调色板应当是 16 色,实际 %d", len(palette))
	}
	for i := 0; i < len(palette); i++ {
		for j := i + 1; j < len(palette); j++ {
			if d := colorDist(palette[i], palette[j]); d < 15 {
				t.Errorf("调色板里 %s 和 %s 太接近:ΔE = %.1f(要求 ≥ 15)",
					palette[i], palette[j], d)
			}
		}
	}
}

// 调色板是按取色顺序排的,而分类列表按创建顺序显示 —— 相邻两个分类拿到的
// 颜色必须差得够远。这条钉住"顺序要交错"这个约定。
func TestPaletteAdjacentAreFar(t *testing.T) {
	for i := 0; i+1 < len(palette); i++ {
		if d := colorDist(palette[i], palette[i+1]); d < 25 {
			t.Errorf("取色顺序里 %s → %s 挨得太近:ΔE = %.1f(要求 ≥ 25)",
				palette[i], palette[i+1], d)
		}
	}
}

func TestPickUnused(t *testing.T) {
	// 全空:给调色板第一个
	if got := pickUnused(map[string]int{}); got != palette[0] {
		t.Errorf("空集应当给 %s,实际 %s", palette[0], got)
	}

	// 前三个都被占了:给第四个
	used := map[string]int{palette[0]: 1, palette[1]: 1, palette[2]: 1}
	if got := pickUnused(used); got != palette[3] {
		t.Errorf("应当给 %s,实际 %s", palette[3], got)
	}

	// 全占满:给用得最少的,而不是按分类数取模。
	// 取模的写法完全没看哪些颜色实际在用 —— 删掉一个分类再新建就会撞色。
	full := map[string]int{}
	for i, c := range palette {
		full[c] = 3
		if i == 7 {
			full[c] = 1 // 只有一个分类用它
		}
	}
	if got := pickUnused(full); got != palette[7] {
		t.Errorf("全占满时应当给用得最少的 %s,实际 %s", palette[7], got)
	}
}

func TestFarthestFrom(t *testing.T) {
	// kept 里已经有一个红色,候选应当避开红系
	got := farthestFrom([]string{"#d0393f"}, map[string]int{})
	if got == "" {
		t.Fatal("不该返回空")
	}
	if d := colorDist(got, "#d0393f"); d < 40 {
		t.Errorf("应当挑一个离已用色很远的,实际 %s(ΔE = %.1f)", got, d)
	}

	// 用过的颜色不该再被选中
	used := map[string]int{}
	for _, c := range palette[:len(palette)-1] {
		used[c] = 1
	}
	if got := farthestFrom(nil, used); got != palette[len(palette)-1] {
		t.Errorf("只剩一个没用过时应当给它,实际 %s", got)
	}
}
