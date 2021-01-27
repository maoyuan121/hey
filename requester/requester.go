// Package requester provides commands to run load tests and display results.
package requester

import (
	"bytes"
	"crypto/tls"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

// Max size of the buffer of result channel.
const maxResult = 1000000
const maxIdleConn = 500

// 测试结果
type result struct {
	err           error // 错误
	statusCode    int   // 状态码
	offset        time.Duration
	duration      time.Duration
	connDuration  time.Duration // connection setup(DNS lookup + Dial up) duration
	dnsDuration   time.Duration // dns 查找时间 dns lookup duration
	reqDuration   time.Duration // 请求的写时间 request "write" duration
	resDuration   time.Duration // 响应的读时间
	delayDuration time.Duration // delay between response and request
	contentLength int64         // 响应内容长度
}

type Work struct {
	// Request is the request to be made.
	Request *http.Request

	// 请求体
	RequestBody []byte

	// N 表示请求总数
	N int

	// C 表示并发数
	C int

	// H2 表示是否是 HTTP/2 请求
	H2 bool

	// 多少秒超时
	Timeout int

	// 1 秒限制多少个请求  Query Per Second
	QPS float64

	// 是否禁用压缩
	DisableCompression bool

	// DisableKeepAlives 表示阻止重用 TCP 连接
	DisableKeepAlives bool

	// DisableRedirects 表示禁止 http 重定向
	DisableRedirects bool

	// Output 代表输出类型。如果值为 csv，那么将输出到 csv 流。
	Output string

	// 代理地址。格式为 "host:port"。可选
	ProxyAddr *url.URL

	// Writer 表示结果输出到哪里。如果为 nil，那么输出到 stdout
	Writer io.Writer

	initOnce sync.Once     // 用来确保只运行一次
	results  chan *result  // 测试结果 chan
	stopCh   chan struct{} // stop chan
	start    time.Duration // 开始时间
	report   *report       // 测试报告
}

// 结果输出到哪里？ 默认输出到控制台
func (b *Work) writer() io.Writer {
	if b.Writer == nil {
		return os.Stdout
	}
	return b.Writer
}

// 初始化内部数据结构
// 初始化 stopCh chan
// 初始化 results chan
func (b *Work) Init() {
	// syncOnce 保证函数只运行一次
	b.initOnce.Do(func() {
		b.results = make(chan *result, min(b.C*1000, maxResult))
		b.stopCh = make(chan struct{}, b.C) // 停止 chan 是每个并发对应一个
	})
}

// 运行所有的请求，打印 summary。会阻塞直至所有的 work 都完成
func (b *Work) Run() {
	b.Init()
	b.start = now()
	b.report = newReport(b.writer(), b.results, b.Output, b.N)
	// Run the reporter first, it polls the result channel until it is closed.
	go func() {
		runReporter(b.report)
	}()
	b.runWorkers()
	b.Finish()
}

// 停止所有请求, 发送 stop signal 给每个并发
func (b *Work) Stop() {
	// 发送 stop signal 给所有的 workers
	for i := 0; i < b.C; i++ {
		b.stopCh <- struct{}{} // stopCh chan struct{}
	}
}

// 结束请求
// 关闭 results chan
// 计算总耗时
// 等待 b.report.done chan
// 打印报告
func (b *Work) Finish() {
	close(b.results)
	total := now() - b.start
	// Wait until the reporter is done.
	<-b.report.done
	b.report.finalize(total)
}

// 发起请求
func (b *Work) makeRequest(c *http.Client) {
	s := now()
	var size int64
	var code int
	var dnsStart, connStart, resStart, reqStart, delayStart time.Duration
	var dnsDuration, connDuration, resDuration, reqDuration, delayDuration time.Duration
	req := cloneRequest(b.Request, b.RequestBody)
	trace := &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			dnsStart = now()
		},
		DNSDone: func(dnsInfo httptrace.DNSDoneInfo) {
			dnsDuration = now() - dnsStart
		},
		GetConn: func(h string) {
			connStart = now()
		},
		GotConn: func(connInfo httptrace.GotConnInfo) {
			if !connInfo.Reused {
				connDuration = now() - connStart
			}
			reqStart = now()
		},
		WroteRequest: func(w httptrace.WroteRequestInfo) {
			reqDuration = now() - reqStart
			delayStart = now()
		},
		GotFirstResponseByte: func() {
			delayDuration = now() - delayStart
			resStart = now()
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := c.Do(req)
	if err == nil {
		size = resp.ContentLength
		code = resp.StatusCode
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}
	t := now()
	resDuration = t - resStart
	finish := t - s
	b.results <- &result{
		offset:        s,
		statusCode:    code,
		duration:      finish,
		err:           err,
		contentLength: size,
		connDuration:  connDuration,
		dnsDuration:   dnsDuration,
		reqDuration:   reqDuration,
		resDuration:   resDuration,
		delayDuration: delayDuration,
	}
}

// 运行 worker
// n = 一个 goroutine 跑多少次请求
func (b *Work) runWorker(client *http.Client, n int) {
	var throttle <-chan time.Time
	if b.QPS > 0 {
		throttle = time.Tick(time.Duration(1e6/(b.QPS)) * time.Microsecond)
	}

	if b.DisableRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	for i := 0; i < n; i++ {
		// Check if application is stopped. Do not send into a closed channel.
		select {
		case <-b.stopCh:
			return
		default:
			if b.QPS > 0 {
				<-throttle
			}
			b.makeRequest(client)
		}
	}
}

func (b *Work) runWorkers() {
	var wg sync.WaitGroup
	wg.Add(b.C) // b.c 是并发量

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         b.Request.Host,
		},
		MaxIdleConnsPerHost: min(b.C, maxIdleConn),
		DisableCompression:  b.DisableCompression,
		DisableKeepAlives:   b.DisableKeepAlives,
		Proxy:               http.ProxyURL(b.ProxyAddr),
	}
	if b.H2 { // 发起 Http 2 请求
		http2.ConfigureTransport(tr)
	} else {
		tr.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	}
	client := &http.Client{Transport: tr, Timeout: time.Duration(b.Timeout) * time.Second}

	// Ignore the case where b.N % b.C != 0.
	for i := 0; i < b.C; i++ {
		go func() {
			b.runWorker(client, b.N/b.C)
			wg.Done()
		}()
	}
	wg.Wait()
}

// 根据入参 http.Request 复制一个 http.Request
func cloneRequest(r *http.Request, body []byte) *http.Request {
	// struct 浅拷贝
	r2 := new(http.Request)
	*r2 = *r

	// 深拷贝 Header
	r2.Header = make(http.Header, len(r.Header))
	for k, s := range r.Header {
		r2.Header[k] = append([]string(nil), s...)
	}

	if len(body) > 0 {
		r2.Body = ioutil.NopCloser(bytes.NewReader(body))
	}
	return r2
}

// 返回 a, b 中值最小的值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
