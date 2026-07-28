package generation

import "testing"

func TestAspectDimensions(t *testing.T) {
	cases := []struct {
		ratio        string
		wantW, wantH int
	}{
		{"1:1", 1024, 1024},
		{"16:9", 1344, 768},
		{"9:16", 768, 1344},
	}
	for _, c := range cases {
		w, h, ok := Dimensions(c.ratio)
		if !ok {
			t.Fatalf("%s 应当被支持", c.ratio)
		}
		if w != c.wantW || h != c.wantH {
			t.Fatalf("%s: got %dx%d, want %dx%d", c.ratio, w, h, c.wantW, c.wantH)
		}
	}
}

func TestAspectRejectsUnknown(t *testing.T) {
	for _, r := range []string{"4:3", "", "1:1 ", "21:9"} {
		if _, _, ok := Dimensions(r); ok {
			t.Fatalf("%q 不该被接受——静默纠正成 1:1 会让前端以为自己传对了", r)
		}
	}
}

func TestAspectDimensionsAreAboutOneMegapixel(t *testing.T) {
	// 上游按输出百万像素计价（实测 cost:7 对应 output_mp:1）。三种画幅都压在
	// 约 1MP，成本才可预测；否则同样"扣 1 次"的两张图真实成本能差一倍。
	for _, r := range []string{"1:1", "16:9", "9:16"} {
		w, h, _ := Dimensions(r)
		mp := float64(w*h) / 1e6
		if mp < 0.9 || mp > 1.2 {
			t.Fatalf("%s 是 %.2fMP，超出 0.9~1.2 区间", r, mp)
		}
	}
}
