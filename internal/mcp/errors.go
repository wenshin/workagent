package mcp

import "errors"

var (
	errServerNotFound        = errors.New("MCP Server 未注册")
	errUnsupportedTransport  = errors.New("仅支持 streamable_http 传输")
)
