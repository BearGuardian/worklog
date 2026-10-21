package store

import (
	"math"
	"strconv"
	"strings"
)

// colorMinDist 是"看着一样"的判定阈值,单位是 CIE76 色差 ΔE。
//
// 参考点:老调色板里的 #7c6bff 和 #8b5cf6 是两个不同的字符串,但肉眼几乎分不出
// (实测 ΔE = 9.1)。只比字符串的"去重"根本不会碰这种,而那恰恰是用户抱怨的东西。
// 阈值取 12 能抓住它,又不会误伤真正不同的颜色 —— 红和橙一般 ΔE 在 30 以上。
const colorMinDist float64 = 12

// parseHex 解析 #RRGGBB。
func parseHex(s string) (r, g, b float64, ok bool) {
	s = strings.TrimSpace(s)
	if len(s) != 7 || s[0] != '#' {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return float64(v>>16&0xff) / 255, float64(v>>8&0xff) / 255, float64(v&0xff) / 255, true
}

// normHex 把颜色统一成小写。
//
// 分类颜色可以由用户在原生取色器里挑,不同浏览器给的大小写不一样 ——
// #7C6BFF 和 #7c6bff 必须算同一个色,否则"挑个没用过的颜色"会挑出重复的。
// 解析不了的原样返回,让上层自己去处理。
func normHex(s string) string {
	s = strings.TrimSpace(s)
	if _, _, _, ok := parseHex(s); !ok {
		return s
	}
	return strings.ToLower(s)
}

// toLab 把 sRGB 转到 CIE L*a*b*(D65 白点)。
func toLab(hex string) ([3]float64, bool) {
	r, g, b, ok := parseHex(hex)
	if !ok {
		return [3]float64{}, false
	}
	r, g, b = srgbToLinear(r), srgbToLinear(g), srgbToLinear(b)

	// sRGB → XYZ,再按白点归一化
	x := (0.4124*r + 0.3576*g + 0.1805*b) / 0.95047
	y := 0.2126*r + 0.7152*g + 0.0722*b
	z := (0.0193*r + 0.1192*g + 0.9505*b) / 1.08883

	fx, fy, fz := labF(x), labF(y), labF(z)
	return [3]float64{116*fy - 16, 500 * (fx - fy), 200 * (fy - fz)}, true
}

func srgbToLinear(c float64) float64 {
	if c <= 0.04045 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

func labF(t float64) float64 {
	if t > 0.008856 {
		return math.Cbrt(t)
	}
	return 7.787*t + 16.0/116
}

// colorDist 返回两色的 CIE76 色差 ΔE。
//
// 认不出来的颜色返回 +Inf —— 调用方拿它做"是不是太像"的判断,
// 一个解析不了的色不该被当成和谁重复。
func colorDist(a, b string) float64 {
	la, ok1 := toLab(a)
	lb, ok2 := toLab(b)
	if !ok1 || !ok2 {
		return math.Inf(1)
	}
	d0, d1, d2 := la[0]-lb[0], la[1]-lb[1], la[2]-lb[2]
	return math.Sqrt(d0*d0 + d1*d1 + d2*d2)
}

// tooClose 判断某个颜色是否和已保留的任何一个"看着一样"。
func tooClose(c string, kept []string) bool {
	for _, k := range kept {
		if colorDist(c, k) < colorMinDist {
			return true
		}
	}
	return false
}

// pickUnused 挑一个没被用过的调色板颜色;都用过了就挑**用得最少**的。
//
// 不能像早先那样拿分类总数取模:那个写法完全没看哪些颜色在实际使用,
// 删掉一个分类再新建就会撞上还留着的颜色。
func pickUnused(used map[string]int) string {
	best := palette[0]
	for _, c := range palette {
		if used[c] == 0 {
			return c
		}
		if used[c] < used[best] {
			best = c
		}
	}
	return best
}

// farthestFrom 从调色板里挑一个离 kept 集合最远的未用色(贪心 max-min)。
//
// 去重时用它:撞色的那个分类换个色,换一个跟已经用过的所有色差别最大的,
// 比"按顺序取下一个"更能拉开距离 —— 顺序取的话可能又取到一个相近的。
func farthestFrom(kept []string, used map[string]int) string {
	best, bestD := "", -1.0
	for _, c := range palette {
		if used[c] > 0 {
			continue
		}
		d := math.Inf(1)
		for _, k := range kept {
			if dd := colorDist(c, k); dd < d {
				d = dd
			}
		}
		if d > bestD {
			best, bestD = c, d
		}
	}
	if best == "" {
		// 调色板用光了,退回"用得最少"的
		return pickUnused(used)
	}
	return best
}
