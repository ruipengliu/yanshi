package query

import (
	"net"
	"net/http"
	"time"
)

// 复用连接池；只收紧建连和 TLS 握手，不把等响应头的正常计算误判为不可达。
var requestTransport = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = time.Second
	return transport
}()

// RequestClient 保留调用方定制 transport 与较短超时；默认 transport 使用短建连预算。
func RequestClient(client *http.Client) http.Client {
	out := http.Client{Timeout: RequestTimeout}
	if client != nil {
		out = *client
	}
	if out.Timeout == 0 || out.Timeout > RequestTimeout {
		out.Timeout = RequestTimeout
	}
	if out.Transport == nil {
		out.Transport = requestTransport
	}
	return out
}
