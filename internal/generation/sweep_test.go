package generation_test

import (
	"testing"

	"image-backend/internal/credit"
	"image-backend/internal/database"
	"image-backend/internal/generation"
	"image-backend/internal/model"
)

func TestSweepRefundsStuckProcessingRows(t *testing.T) {
	db, err := database.Open("")
	if err != nil {
		t.Fatal(err)
	}
	u := model.User{Email: "sweep@example.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	if err := credit.Grant(db, u.ID, 10, 0, "fixture"); err != nil {
		t.Fatal(err)
	}
	gen := model.Generation{
		ID: "stuck-1", UserID: u.ID, Model: "flux-2-max", Prompt: "p",
		AspectRatio: "1:1", Width: 1024, Height: 1024, Status: model.GenStatusProcessing,
	}
	if err := db.Create(&gen).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := credit.Spend(db, u.ID, 3, gen.ID); err != nil {
		t.Fatal(err)
	}

	n, err := generation.SweepStuck(db)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("应当回收 1 行: got %d", n)
	}
	bal, _ := credit.Balance(db, u.ID)
	if bal.MonthlyCredits != 10 {
		t.Fatalf("次数应当退回: got %d, want 10", bal.MonthlyCredits)
	}
	var after model.Generation
	_ = db.Where("id = ?", gen.ID).First(&after).Error
	if after.Status != model.GenStatusFailed {
		t.Fatalf("行应当标成 failed: got %s", after.Status)
	}
}

func TestSweepIsIdempotent(t *testing.T) {
	db, _ := database.Open("")
	u := model.User{Email: "sweep2@example.com", PasswordHash: "x"}
	_ = db.Create(&u).Error
	_ = credit.Grant(db, u.ID, 10, 0, "fixture")
	gen := model.Generation{
		ID: "stuck-2", UserID: u.ID, Model: "flux-2-max", Prompt: "p",
		AspectRatio: "1:1", Width: 1024, Height: 1024, Status: model.GenStatusProcessing,
	}
	_ = db.Create(&gen).Error
	_, _ = credit.Spend(db, u.ID, 3, gen.ID)

	// 每次重启都会跑，所以重跑必须安全。
	if _, err := generation.SweepStuck(db); err != nil {
		t.Fatal(err)
	}
	if n, err := generation.SweepStuck(db); err != nil || n != 0 {
		t.Fatalf("第二次不该再回收: n=%d err=%v", n, err)
	}
	bal, _ := credit.Balance(db, u.ID)
	if bal.MonthlyCredits != 10 {
		t.Fatalf("重复扫描导致多退: got %d, want 10", bal.MonthlyCredits)
	}
}

func TestSweepLeavesTerminalRowsAlone(t *testing.T) {
	db, _ := database.Open("")
	u := model.User{Email: "sweep3@example.com", PasswordHash: "x"}
	_ = db.Create(&u).Error
	_ = credit.Grant(db, u.ID, 10, 0, "fixture")
	done := model.Generation{
		ID: "done-1", UserID: u.ID, Model: "flux-2-max", Prompt: "p",
		AspectRatio: "1:1", Width: 1024, Height: 1024,
		Status: model.GenStatusSucceeded, CreditsSpent: 1,
	}
	_ = db.Create(&done).Error
	_, _ = credit.Spend(db, u.ID, 1, done.ID)

	n, err := generation.SweepStuck(db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("已终态的行不该被回收: got %d", n)
	}
	bal, _ := credit.Balance(db, u.ID)
	if bal.MonthlyCredits != 9 {
		t.Fatalf("成功的生成不该被退款: got %d, want 9", bal.MonthlyCredits)
	}
}
