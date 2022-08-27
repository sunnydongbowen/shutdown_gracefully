package service

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var wg sync.WaitGroup

// 典型的 Option 设计模式
type Option func(*App)

// ShutdownCallback 采用 context.Context 来控制超时，而不是用 time.After 是因为
// - 超时本质上是使用这个回调的人控制的
// - 我们还希望用户知道，他的回调必须要在一定时间内处理完毕，而且他必须显式处理超时错误
type ShutdownCallback func(ctx context.Context)

// 你需要实现这个方法 注册退出/关闭时需要执行的函数。注册退出/关闭时需要执行的函数。
// 回调函数有多个，所以这里
func WithShutdownCallbacks(cbs ...ShutdownCallback) Option {
	return func(app *App) {
		app.cbs = cbs
	}
}

// 这里我已经预先定义好了各种可配置字段v App是一个结构体，里面设置了各种字段
type App struct {
	servers []*Server

	// 优雅退出整个超时时间，默认30秒，需要我们自己初始化的时候设置
	shutdownTimeout time.Duration

	// 优雅退出时候等待处理已有请求时间，默认10秒钟
	waitTime time.Duration

	// 自定义回调超时时间，默认三秒钟
	cbTimeout time.Duration

	//
	cbs []ShutdownCallback
	//
}

// NewApp 创建 App 实例，注意设置默认值，同时使用这些选项
// 传参: sever类型的服务列表 ,Option类型的函数，可以传多个.使用的時候传的是.WithShutdownCallbacks。这个就是返回值为Option类型的函数,并且这个函数的参数cbs类型的
// 返回值
func NewApp(servers []*Server, opts ...Option) *App {
	// 先初始化了前面的几个字段
	app := &App{
		servers:         servers, //调用这个函数的人传什么就是什么啊，这个就是给调用的传的，你看参数在里面呢。至于具体的server有哪些结构体,暂时不用关心。
		shutdownTimeout: time.Second * 30,
		waitTime:        time.Second * 10,
		cbTimeout:       time.Second * 3,
	}
	// 初始化cbs，cbs是一个关闭前的回调函数，所以这里我们用slice方式进行初始化，因为可能不止一个回调函数
	//
	for _, opt := range opts {
		opt(app)
	}
	return app
}

// StartAndServe 你主要要实现这个方法
func (app *App) StartAndServe() {
	for _, s := range app.servers {
		srv := s
		go func() {
			if err := srv.Start(); err != nil {
				if err == http.ErrServerClosed {
					log.Printf("服务器%s已关闭", srv.name)
				} else {
					log.Printf("服务器%s异常退出", srv.name)
				}
			}
		}()
	}
	// 从这里开始优雅退出监听系统信号，强制退出以及超时强制退出。
	// 优雅退出的具体步骤在 shutdown 里面实现
	// 所以你需要在这里恰当的位置，调用 shutdown
	ch := make(chan os.Signal, 1) // 定义一个信号类型的channel
	// 定义监听的信号
	signals := []os.Signal{syscall.SIGTERM, syscall.SIGINT} // 定义ctr+c和kill信号
	signal.Notify(ch, signals...)                           //  监听信号

	//ctx, cancel := context.WithTimeout(ctx)
	select {
	case <-ch:
		go func() {
			select {
			case <-ch:
				log.Printf("强制退出")
				os.Exit(1)
			case <-time.After(app.shutdownTimeout):
				log.Printf("超时强制退出")
				os.Exit(1)
			}
		}()
		// app。shutdown
		app.shutdown()
	}
	//if 收到新信号 {
	//	app.shutdown()
	//}
}

// shutdown 你要设计这里面的执行步骤。
func (app *App) shutdown() {
	log.Println("开始关闭应用，停止接收新请求")
	// 你需要在这里让所有的 server 拒绝新请求
	// 停止接收新请求
	for _, server := range app.servers {
		server.rejectReq() // 拒绝新请求
	}

	log.Println("等待正在执行请求完结")
	// 在这里等待一段时间
	time.Sleep(app.waitTime)

	log.Println("开始关闭服务器")
	// 并发关闭服务器，同时要注意协调所有的 server 都关闭之后才能步入下一个阶段
	wg.Add(len(app.servers))
	for _, srv := range app.servers {
		srvCp := srv
		go func() {
			if err := srvCp.stop(); err != nil {
				log.Printf("关闭服务失败%s\n")
			}
			wg.Done()
		}()
	}
	wg.Wait()

	log.Println("开始执行自定义回调")
	// 并发执行回调，要注意协调所有的回调都执行完才会步入下一个阶段
	wg.Add(len(app.cbs))
	for _, cb := range app.cbs {
		c := cb
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), app.cbTimeout)
			ctx.Done()
			c(ctx)
			cancel()
		}()
	}
	// 释放资源
	log.Println("开始释放资源")
	app.close()
}

func (app *App) close() {
	// 在这里释放掉一些可能的资源
	time.Sleep(time.Second)
	log.Println("应用关闭")
}

// serverMux 既可以看做是装饰器模式，也可以看做委托模式
type serverMux struct {
	reject bool
	*http.ServeMux
}

type Server struct {
	srv  *http.Server
	name string
	mux  *serverMux
}

// 拒绝新请求
func (s *serverMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.reject {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("服务已关闭"))
		return
	}
	s.ServeMux.ServeHTTP(w, r)
}

func NewServer(name string, addr string) *Server {
	mux := &serverMux{ServeMux: http.NewServeMux()}
	return &Server{
		name: name,
		mux:  mux,
		srv: &http.Server{
			Addr:    addr,
			Handler: mux,
		},
	}
}

func (s *Server) Handle(pattern string, handler http.Handler) {
	s.mux.Handle(pattern, handler)
}

func (s *Server) Start() error {
	return s.srv.ListenAndServe()
}

func (s *Server) rejectReq() {
	s.mux.reject = true
}

func (s *Server) stop() error {
	log.Printf("服务器%s关闭中", s.name)
	return s.srv.Shutdown(context.Background())
}
