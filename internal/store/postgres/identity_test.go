package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/workagent/auto-agent/internal/store"
)

// TestUpsertUserByIdentityRoundTrip 校验身份映射与「有值才覆盖」的降级语义。
func TestUpsertUserByIdentityRoundTrip(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 DATABASE_URL，跳过集成测试")
	}

	ctx := context.Background()
	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(db.Close)

	uid := "itest-union-" + t.Name()
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `delete from app_user where id in (select user_id from user_identity where provider_uid = $1)`, uid)
	})

	first, err := db.UpsertUserByIdentity(ctx, store.IdentityProfile{
		Provider: "dingtalk", ProviderUID: uid, UnionID: uid,
		Name: "集成测试", Mobile: "13800000000",
		Email: "itest@corp.com", JobNumber: "IT001", Title: "工程师",
		CorpName: "测试企业",
		Raw:      []byte(`{"userid":"u-itest"}`),
	})
	if err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	if first.Email != "itest@corp.com" || first.JobNumber != "IT001" || first.CorpName != "测试企业" {
		t.Fatalf("工号/邮箱/企业名未落库: %+v", first)
	}
	if first.Provider != "dingtalk" || first.ProviderUID != uid {
		t.Fatalf("身份来源不符: %+v", first)
	}
	if first.ProfileWarning != "" {
		t.Fatalf("同步成功时不应有告警: %q", first.ProfileWarning)
	}

	// 通讯录权限缺失：本次没有工号/邮箱/企业名，不应清空已存值，但要记录失败原因
	second, err := db.UpsertUserByIdentity(ctx, store.IdentityProfile{
		Provider: "dingtalk", ProviderUID: uid, UnionID: uid,
		Name: "集成测试改名", Mobile: "13900000000",
		ContactError: "企业资料未同步：应用缺少通讯录成员读权限（qyapi_get_member）",
	})
	if err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("同一身份不应产生新用户: %s != %s", second.ID, first.ID)
	}
	if second.Email != "itest@corp.com" || second.JobNumber != "IT001" || second.CorpName != "测试企业" {
		t.Fatalf("降级时不应清空工号/邮箱/企业名: %+v", second)
	}
	if second.Name != "集成测试改名" || second.Mobile != "13900000000" {
		t.Fatalf("新资料未更新: %+v", second)
	}
	if second.ProfileWarning == "" {
		t.Fatal("降级原因应落库并返回给前端提示")
	}

	// 权限恢复后再次带值写入：覆盖资料并清空历史告警
	third, err := db.UpsertUserByIdentity(ctx, store.IdentityProfile{
		Provider: "dingtalk", ProviderUID: uid, UnionID: uid,
		Email: "new@corp.com", JobNumber: "IT002",
	})
	if err != nil {
		t.Fatalf("三次写入失败: %v", err)
	}
	if third.Email != "new@corp.com" || third.JobNumber != "IT002" {
		t.Fatalf("有值时应覆盖: %+v", third)
	}
	if third.ProfileWarning != "" {
		t.Fatalf("同步成功后应清空告警，实际 %q", third.ProfileWarning)
	}

	got, err := db.GetUser(ctx, first.ID)
	if err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if got.Email != "new@corp.com" || got.JobNumber != "IT002" || got.Provider != "dingtalk" {
		t.Fatalf("查询结果与写入不一致: %+v", got)
	}
}
