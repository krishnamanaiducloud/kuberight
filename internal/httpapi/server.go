package httpapi

import (
 "encoding/json"
 "github.com/kuberight/kuberight/internal/optimizer"
 "log/slog"
 "net/http"
 "time"
)
func Handler() http.Handler {
 mux:=http.NewServeMux()
 mux.HandleFunc("GET /healthz",func(w http.ResponseWriter,r *http.Request){w.WriteHeader(200);_,_=w.Write([]byte("ok"))})
 mux.HandleFunc("GET /readyz",func(w http.ResponseWriter,r *http.Request){w.WriteHeader(200);_,_=w.Write([]byte("ready"))})
 mux.HandleFunc("GET /api/v1/version",func(w http.ResponseWriter,r *http.Request){respond(w,200,map[string]string{"version":"1.0.0","mode":"recommendation-only"})})
 mux.HandleFunc("POST /api/v1/recommendations:calculate",func(w http.ResponseWriter,r *http.Request){
  r.Body=http.MaxBytesReader(w,r.Body,1<<20);defer r.Body.Close()
  d:=json.NewDecoder(r.Body);d.DisallowUnknownFields()
  var in optimizer.Input
  if err:=d.Decode(&in);err!=nil{respond(w,400,map[string]string{"error":"invalid input"});return}
  out,err:=optimizer.Recommend(in);if err!=nil{respond(w,422,map[string]string{"error":err.Error()});return}
  respond(w,200,out)
 })
 auth,authErr:=configuredAuth()
 return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  w.Header().Set("X-Content-Type-Options","nosniff");w.Header().Set("Cache-Control","no-store")
  if r.URL.Path=="/healthz"||r.URL.Path=="/readyz"{mux.ServeHTTP(w,r);return}
  if authErr!=nil{respond(w,503,map[string]string{"error":"authentication unavailable"});return}
  bearer,ok:=extractBearer(r.Header.Get("Authorization"))
  if !ok{w.Header().Set("WWW-Authenticate",`Bearer realm="kuberight"`);respond(w,401,map[string]string{"error":"unauthorized"});return}
  verb:="get"
  if r.Method!="GET"&&r.Method!="HEAD"{verb="create"}
  status,err:=auth.Authenticate(r.Context(),bearer,verb)
  if err!=nil{respond(w,status,map[string]string{"error":err.Error()});return}
  slog.Info("request","method",r.Method,"path",r.URL.Path)
  mux.ServeHTTP(w,r)
 })
}
func respond(w http.ResponseWriter,code int,v any){w.Header().Set("Content-Type","application/json");w.WriteHeader(code);_=json.NewEncoder(w).Encode(v)}
func Server(addr string)*http.Server{return &http.Server{Addr:addr,Handler:Handler(),ReadHeaderTimeout:5*time.Second,ReadTimeout:15*time.Second,WriteTimeout:20*time.Second,IdleTimeout:60*time.Second}}
