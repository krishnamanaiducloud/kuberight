package httpapi

import (
 "context"
 "crypto/subtle"
 "errors"
 "net/http"
 "os"
 "strings"
 "time"

 authnv1 "k8s.io/api/authentication/v1"
 authzv1 "k8s.io/api/authorization/v1"
 metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
 "k8s.io/client-go/kubernetes"
 "k8s.io/client-go/rest"
)

type Authenticator interface { Authenticate(context.Context,string,string)(int,error) }
type staticAuth struct { token string }
func (a staticAuth) Authenticate(_ context.Context,bearer,_ string)(int,error){
 if a.token==""{return http.StatusServiceUnavailable,errors.New("authentication unavailable")}
 if subtle.ConstantTimeCompare([]byte(bearer),[]byte(a.token))!=1{return http.StatusUnauthorized,errors.New("unauthorized")}
 return http.StatusOK,nil
}
type kubeAuth struct{client kubernetes.Interface}
func (a kubeAuth) Authenticate(ctx context.Context,bearer,verb string)(int,error){
 ctx,cancel:=context.WithTimeout(ctx,5*time.Second);defer cancel()
 tr,err:=a.client.AuthenticationV1().TokenReviews().Create(ctx,&authnv1.TokenReview{Spec:authnv1.TokenReviewSpec{Token:bearer}},metav1.CreateOptions{})
 if err!=nil{return http.StatusServiceUnavailable,errors.New("identity verification unavailable")}
 if !tr.Status.Authenticated||tr.Status.Error!=""||tr.Status.User.Username==""{return http.StatusUnauthorized,errors.New("unauthorized")}
 extra:=map[string]authzv1.ExtraValue{}
 for k,v:=range tr.Status.User.Extra{extra[k]=authzv1.ExtraValue(v)}
 sar,err:=a.client.AuthorizationV1().SubjectAccessReviews().Create(ctx,&authzv1.SubjectAccessReview{Spec:authzv1.SubjectAccessReviewSpec{
 User:tr.Status.User.Username,UID:tr.Status.User.UID,Groups:tr.Status.User.Groups,Extra:extra,
 ResourceAttributes:&authzv1.ResourceAttributes{Group:"kuberight.io",Resource:"recommendations",Verb:verb},
 }},metav1.CreateOptions{})
 if err!=nil||sar.Status.EvaluationError!=""{return http.StatusServiceUnavailable,errors.New("authorization unavailable")}
 if sar.Status.Denied||!sar.Status.Allowed{return http.StatusForbidden,errors.New("forbidden")}
 return http.StatusOK,nil
}
func configuredAuth()(Authenticator,error){
 switch os.Getenv("KUBERIGHT_AUTH_MODE"){
 case "static":return staticAuth{token:os.Getenv("KUBERIGHT_API_TOKEN")},nil
 case "kubernetes":
  cfg,err:=rest.InClusterConfig();if err!=nil{return nil,err}
  client,err:=kubernetes.NewForConfig(cfg);if err!=nil{return nil,err}
  return kubeAuth{client:client},nil
 default:return nil,errors.New("authentication mode must be explicitly configured")
 }
}
func extractBearer(h string)(string,bool){
 p:=strings.Fields(h)
 returnValue:=""
 if len(p)==2&&strings.EqualFold(p[0],"Bearer")&&len(p[1])<=16384{returnValue=p[1]}
 return returnValue,returnValue!=""
}
