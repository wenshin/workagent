package tool

import "testing"

// shellNeedsNetwork 决定「沙箱给不给网」，而给网意味着工作区不可见。
// 所以判错的代价是不对称的：判多只是白弹一次审批，判少是命令直接失败。
func TestShellNeedsNetwork(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		// 纯本地操作：不该要网
		{"ls -la", false},
		{"grep -rn foo .", false},
		{"cat a.txt | base64", false},
		{"mv a b && rm -rf c", false},
		{"python3 -m venv .deps/venv", false},
		{"python3 -V", false},
		{"node -v", false},
		{"go version", false},
		{"npm run build", false},
		{"npm ls", false},

		// 包管理器安装：没网 100% 失败
		{"pip install requests", true},
		{"pip3 download pandas", true},
		{"python3 -m pip install requests", true},
		{"npm i", true},
		{"npm install --save-dev typescript", true},
		{"pnpm add lodash", true},
		{"fnm install 20", true},
		{"go build ./...", true},
		{"cargo build", true},

		// 网络工具
		{"curl -o x.sh https://example.com/i.sh", true},
		{"wget https://example.com/a.tar.gz", true},
		{"nc 1.2.3.4 80", true},

		// bash 内置通道：不需要任何外部二进制
		{"exec 3<>/dev/tcp/e.co/80", true},

		// 脚本解释器：看不到脚本内容，按需要网处理
		{"python3 script.py", true},
		{"node app.js", true},
		{"bash -c 'ls'", true},
		{".deps/node/bin/node -e 'console.log(1)'", true},
	}
	for _, c := range cases {
		if got := shellNeedsNetwork(c.cmd); got != c.want {
			t.Errorf("shellNeedsNetwork(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

// 子串误匹配会把 `django` 当成 `go`、把 `logo.py` 当成 `go`。
// 这条只验证常用的几个短词没有落在普通文件名里。
func TestShellNeedsNetworkNoSubstringFalsePositive(t *testing.T) {
	for _, cmd := range []string{
		"ls django/",
		"cat logo.txt",
		"grep -rn pythonista .",
		"wc -l shell.md",
	} {
		if shellNeedsNetwork(cmd) {
			t.Errorf("shellNeedsNetwork(%q) 不该为 true", cmd)
		}
	}
}

// 审批判据比出网判据窄：装依赖要网但不弹确认，
// 只有「目标由命令内容决定」的命令才需要人看一眼。
func TestShellNeedsApproval(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		// 包管理器安装：要网，但不审批
		{"pip install requests", false},
		{"pip3 download pandas", false},
		{"python3 -m pip install requests", false},
		{"npm i", false},
		{"npm install --save-dev typescript", false},
		{"pnpm add lodash", false},
		{"fnm install 20", false},
		{"uv sync", false},
		// 包名里带 node / python 不能误判
		{"npm i node-fetch", false},
		{"pip install python-dateutil", false},
		{"npm i django", false},

		// 网络工具：目标在命令里，审批
		{"curl -o x.sh https://example.com/i.sh", true},
		{"wget https://example.com/a.tar.gz", true},
		{"nc 1.2.3.4 80", true},
		{"exec 3<>/dev/tcp/e.co/80", true},

		// 脚本解释器：看不到脚本内容，审批
		{"python3 script.py", true},
		{"node app.js", true},
		{"bash -c 'ls'", true},
		{".deps/node/bin/node -e 'console.log(1)'", true},

		// 复合命令：只要有一段危险就审批
		{"curl https://x.co/a && pip install requests", true},
		{"pip install requests; curl https://x.co/a", true},

		// 纯本地与「确实不需要网」的形态
		{"ls -la", false},
		{"python3 -m venv .venv", false},
		{"python3 -V", false},
		{"node -v", false},
		{"go version", false},
		{"npm run build", false},
		{"npm ls", false},
	}
	for _, c := range cases {
		if got := shellNeedsApproval(c.cmd); got != c.want {
			t.Errorf("shellNeedsApproval(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

// 两条判据必须能同时为真：装依赖「给网但不审批」，
// 出网时只挂 .deps 才装得进去。
func TestPkgInstallNeedsNetworkButNotApproval(t *testing.T) {
	for _, cmd := range []string{"pip install requests", "npm i", "fnm install 20"} {
		if !shellNeedsNetwork(cmd) {
			t.Errorf("%q 必须判为需要出网，否则装不上", cmd)
		}
		if shellNeedsApproval(cmd) {
			t.Errorf("%q 不该弹审批", cmd)
		}
	}
}
