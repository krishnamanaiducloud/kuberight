package main

import (
 "context"
 "errors"
 "github.com/kuberight/kuberight/internal/httpapi"
 "log/slog"
 "net/http"
 "os"
 "os/signal"
 "syscall"
 "time"
)
func main() {
 addr:=os.Getenv("KUBERIGHT_LISTEN_ADDR"); if addr=="" {addr=":8080"}
 srv:=httpapi.Server(addr)
 ctx,stop:=signal.NotifyContext(context.Background(),syscall.SIGTERM,syscall.SIGINT);defer stop()
 go func(){<-ctx.Done();c,cancel:=context.WithTimeout(context.Background(),10*time.Second);defer cancel();_ = srv.Shutdown(c)}()
 slog.Info("starting KubeRight v1.0.0","address",addr)
 if err:=srv.ListenAndServe();err!=nil&&!errors.Is(err,http.ErrServerClosed){slog.Error("server failed","error",err);os.Exit(1)}
}
