package plugin

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func agentManifest(extra string) string {
	return `{
  "protocol": "wa-plugin/1",
  "kind": "agent",
  "slug": "reviewer",
  "name": "代码评审",
  "description": "按团队规范做代码评审",
  "agent": {"entry": "AGENT.md"` + extra + `}
}`
}

// 头像路径只能是插件目录内的图片文件：跳出目录、指向清单、非图片扩展名都要拒绝，
// 否则一个手写清单就能让「头像」变成任意文件读取。
func TestAgentIconPathValidation(t *testing.T) {
	for _, p := range []string{"icon.png", "assets/logo.png", "a.webp", "b.JPG"} {
		if err := validateAgentIcon(p); err != nil {
			t.Errorf("%q 应该合法: %v", p, err)
		}
	}
	for _, p := range []string{"../x.png", "a/../../b.png", "/abs.png", "plugin.json", "icon.svg", "icon.txt", "a/../b.png", ".."} {
		if err := validateAgentIcon(p); err == nil {
			t.Errorf("%q 应该被拒绝", p)
		}
	}
}

func TestAgentManifestIcon(t *testing.T) {
	ok, err := Parse([]byte(agentManifest(`, "icon": "icon.png"`)), false)
	if err != nil {
		t.Fatalf("声明头像的清单应解析成功: %v", err)
	}
	if ok.Agent == nil || ok.Agent.Icon != "icon.png" {
		t.Fatalf("头像字段没保留: %+v", ok.Agent)
	}

	if _, err := Parse([]byte(agentManifest(`, "icon": "../escape.png"`)), false); err == nil {
		t.Fatal("越界头像路径应被拒绝")
	}
}

// 400×200 的图居中裁出 [100,300) 后红色左条整条被裁掉：产物必须是 256×256 全蓝。
func TestNormalizeAgentIconCropsCenter(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 400, 200))
	red := color.NRGBA{R: 0xff, A: 0xff}
	blue := color.NRGBA{B: 0xff, A: 0xff}
	for y := 0; y < 200; y++ {
		for x := 0; x < 400; x++ {
			if x < 100 {
				src.SetNRGBA(x, y, red)
				continue
			}
			src.SetNRGBA(x, y, blue)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("构造测试图失败: %v", err)
	}

	out, err := normalizeAgentIcon(buf.Bytes())
	if err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	if len(out) > maxAgentIconBytes {
		t.Fatalf("产物超过上限: %d", len(out))
	}

	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("产物不是 PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != agentIconSide || b.Dy() != agentIconSide {
		t.Fatalf("尺寸应为 %d×%d，实际 %d×%d", agentIconSide, agentIconSide, b.Dx(), b.Dy())
	}
	for _, p := range []image.Point{
		{0, 0}, {agentIconSide - 1, 0}, {0, agentIconSide - 1}, {agentIconSide - 1, agentIconSide - 1},
	} {
		r, g, b, _ := img.At(p.X, p.Y).RGBA()
		if b>>8 < 0xf0 || r>>8 > 0x0f || g>>8 > 0x0f {
			t.Fatalf("%v 应为中心裁剪后的蓝色，实际 r=%d g=%d b=%d", p, r>>8, g>>8, b>>8)
		}
	}
}

func TestNormalizeAgentIconRejectsInvalid(t *testing.T) {
	if _, err := normalizeAgentIcon(nil); err == nil {
		t.Fatal("空文件应被拒绝")
	}
	if _, err := normalizeAgentIcon([]byte("not an image")); err == nil {
		t.Fatal("非图片应被拒绝")
	}
}
