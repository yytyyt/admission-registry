package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"github.com/yytyyt/admission-registry/pkg"

	"k8s.io/klog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	var param pkg.WhSrvParam
	// webhook http server (tls)
	// 命令行参数
	flag.IntVar(&param.Port, "port", 443, "Webhook Server Port")
	flag.StringVar(&param.CertFile, "tlsCertFile", "/etc/webhook/certs/tls.crt", "x509 certification file")
	flag.StringVar(&param.KeyFile, "tlsKeyFile", "/etc/webhook/certs/tls.key", "x509 private key file")
	flag.Parse()

	certificate, err := tls.LoadX509KeyPair(param.CertFile, param.KeyFile)
	if err != nil {
		klog.Errorf("Failed to load key pair:%v", err)
		return
	}
	webhookServer := pkg.WebhookServer{
		Server: &http.Server{
			Addr: fmt.Sprintf(":%d", param.Port),
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{
					certificate,
				},
			},
		},
		WhiteListRegistries: strings.Split(os.Getenv("WHITELIST_REGISTRIES"), ","),
	}

	// 定义 http server handler
	mux := http.NewServeMux()
	mux.HandleFunc("/validate", webhookServer.Handler)
	mux.HandleFunc("/mutate", webhookServer.Handler)

	webhookServer.Server.Handler = mux

	// 在一个新的 gorouting 里面去启动 webhook server
	go func() {
		if err = webhookServer.Server.ListenAndServeTLS("", ""); err != nil {
			klog.Errorf("Failed to listen and serve webhook:%v", err)
		}
	}()

	klog.Info("Server started")

	// 监听OS的关闭信号
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	<-signalChan

	klog.Info("Get OS shutdown signal")

	if err = webhookServer.Server.Shutdown(context.Background()); err != nil {
		klog.Errorf("HTTP Server Shutdown err:%v", err)
	}
}
