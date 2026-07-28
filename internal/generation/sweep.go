package generation

import (
	"log"

	"gorm.io/gorm"

	"image-backend/internal/credit"
	"image-backend/internal/model"
)

// SweepStuck 回收卡在 processing 的生成任务，返回回收数量。
//
// 何时会有卡住的行：生成是同步的，服务端要挂住连接直到上游出图（Flux 实测 21
// 秒）。这期间进程崩溃或部署重启，那一行就永远停在 processing——次数扣了、图
// 没有、没有任何人会来收拾。异步方案有 worker 兜底，同步方案没有，所以这个扫描
// 不是可选项。
//
// **必须在开始接收流量之前调用。** 那时候任何 processing 行按定义都是上一个进程
// 遗留的孤儿；服务跑起来之后再扫，会把当前正在进行的生成误判成孤儿并退款。
//
// 幂等由 credit.Refund 保证（(generation_id, type) 唯一索引），每次重启都跑是安全的。
func SweepStuck(db *gorm.DB) (int, error) {
	var stuck []model.Generation
	if err := db.Where("status = ?", model.GenStatusProcessing).Find(&stuck).Error; err != nil {
		return 0, err
	}
	for _, g := range stuck {
		if err := credit.Refund(db, g.ID); err != nil {
			// 单行失败不中断整体——剩下的孤儿更值得被回收。留痕即可，下次重启还会再试。
			log.Printf("[sweep] 退款失败 gen=%s: %v", g.ID, err)
			continue
		}
		if err := db.Model(&model.Generation{}).Where("id = ?", g.ID).
			Updates(map[string]any{
				"status": model.GenStatusFailed,
				"error":  "服务重启时该任务仍在进行中，次数已退回",
			}).Error; err != nil {
			log.Printf("[sweep] 标记失败 gen=%s: %v", g.ID, err)
		}
	}
	if len(stuck) > 0 {
		log.Printf("[sweep] 回收了 %d 个卡住的生成任务", len(stuck))
	}
	return len(stuck), nil
}
