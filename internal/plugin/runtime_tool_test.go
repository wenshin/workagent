package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
)

type fakeSecrets struct{ values map[string]string }

func (f fakeSecrets) Get(_ context.Context, scope, name string) (string, error) {
	if v, ok := f.values[scope+"/"+name]; ok {
		return v, nil
	}
	return "", errors.New("不存在")
}

func secretTestRuntime() *toolRuntime {
	return &toolRuntime{
		secrets: fakeSecrets{values: map[string]string{"plugin/erp_token": "s3cr3t"}},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// 声明过的名字才能取到值。
func TestSecretGetReturnsDeclared(t *testing.T) {
	rt := secretTestRuntime()
	in := invokeInput{Secrets: map[string]bool{"erp_token": true}}

	out, err := rt.secretGet(context.Background(), in, json.RawMessage(`{"name":"erp_token"}`))
	if err != nil {
		t.Fatalf("取凭证失败: %v", err)
	}
	got, _ := out.(map[string]any)
	if got["value"] != "s3cr3t" {
		t.Fatalf("凭证值 = %v，期望 s3cr3t", got["value"])
	}
}

// 没声明的名字必须被拒绝，不能因为它恰好存在就放行——
// 否则「写了 secret.use」就等于「能读整个 plugin scope」。
func TestSecretGetRejectsUndeclared(t *testing.T) {
	rt := secretTestRuntime()
	in := invokeInput{Secrets: map[string]bool{"erp_token": true}}

	if _, err := rt.secretGet(context.Background(), in, json.RawMessage(`{"name":"other_token"}`)); err == nil {
		t.Fatal("未声明的凭证名应该被拒绝")
	}
}

// 声明了但凭证不存在：报错要能定位，而不是返回空串让请求带着半个凭证出去。
func TestSecretGetMissingValue(t *testing.T) {
	rt := secretTestRuntime()
	in := invokeInput{Secrets: map[string]bool{"absent": true}}

	if _, err := rt.secretGet(context.Background(), in, json.RawMessage(`{"name":"absent"}`)); err == nil {
		t.Fatal("不存在的凭证应该报错")
	}
}
