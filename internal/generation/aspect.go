package generation

// Dimensions 把画幅映射成具体宽高。第二个返回值为 false 表示不支持该画幅。
//
// **不做静默纠正。** 传了不支持的画幅就该报错：静默改成 1:1 会让前端以为自己
// 传对了，而用户拿到的是另一个比例的图，且没有任何地方提示出了问题。
//
// 三种尺寸都压在约 1MP：上游按输出百万像素计价（实测 cost:7 ↔ output_mp:1），
// 尺寸浮动会让"扣 1 次"对应的真实成本浮动。
func Dimensions(aspectRatio string) (width, height int, ok bool) {
	switch aspectRatio {
	case "1:1":
		return 1024, 1024, true
	case "16:9":
		return 1344, 768, true
	case "9:16":
		return 768, 1344, true
	default:
		return 0, 0, false
	}
}
