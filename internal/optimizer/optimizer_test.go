package optimizer
import "testing"
func TestRecommend(t *testing.T){
 r,err:=Recommend(Input{Namespace:"default",Workload:"demo",Container:"app",CPURequestsMilli:1000,MemoryRequestsBytes:1073741824,CPUSamplesMilli:[]float64{100,200},MemorySamplesBytes:[]float64{100000000,200000000}})
 if err!=nil{t.Fatal(err)}
 if r.Applied{t.Fatal("must not apply")}
 if r.CPURequestMilli<800{t.Fatal("unsafe reduction")}
}
