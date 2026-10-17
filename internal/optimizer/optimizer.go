package optimizer

import ("errors";"math";"sort")
type Input struct {
 Namespace string `json:"namespace"`
 Workload string `json:"workload"`
 Container string `json:"container"`
 CPURequestsMilli int64 `json:"cpuRequestMilli"`
 MemoryRequestsBytes int64 `json:"memoryRequestBytes"`
 CPUSamplesMilli []float64 `json:"cpuSamplesMilli"`
 MemorySamplesBytes []float64 `json:"memorySamplesBytes"`
}
type Recommendation struct {
 Namespace string `json:"namespace"`
 Workload string `json:"workload"`
 Container string `json:"container"`
 CPURequestMilli int64 `json:"cpuRequestMilli"`
 MemoryRequestBytes int64 `json:"memoryRequestBytes"`
 CurrentCPURequestMilli int64 `json:"currentCpuRequestMilli"`
 CurrentMemoryRequestBytes int64 `json:"currentMemoryRequestBytes"`
 Confidence string `json:"confidence"`
 Reason string `json:"reason"`
 Applied bool `json:"applied"`
}
func percentile(values []float64,q float64)(float64,error){
 if len(values)==0{return 0,errors.New("no samples")}
 a:=make([]float64,0,len(values))
 for _,v:=range values{if math.IsNaN(v)||math.IsInf(v,0)||v<0{return 0,errors.New("invalid sample")};a=append(a,v)}
 sort.Float64s(a);pos:=q*float64(len(a)-1);lo:=int(math.Floor(pos));hi:=int(math.Ceil(pos))
 return a[lo]+(a[hi]-a[lo])*(pos-float64(lo)),nil
}
func Recommend(in Input)(Recommendation,error){
 if in.Namespace==""||in.Workload==""||in.Container==""{return Recommendation{},errors.New("namespace, workload and container are required")}
 if in.CPURequestsMilli<0||in.MemoryRequestsBytes<0{return Recommendation{},errors.New("requests cannot be negative")}
 cpu,err:=percentile(in.CPUSamplesMilli,.95);if err!=nil{return Recommendation{},err}
 mem,err:=percentile(in.MemorySamplesBytes,.99);if err!=nil{return Recommendation{},err}
 cpuReq:=int64(math.Ceil(math.Max(50,cpu*1.30)))
 memReq:=int64(math.Ceil(math.Max(64*1024*1024,mem*1.25)))
 if in.CPURequestsMilli>0&&cpuReq<int64(math.Ceil(float64(in.CPURequestsMilli)*.8)){cpuReq=int64(math.Ceil(float64(in.CPURequestsMilli)*.8))}
 if in.MemoryRequestsBytes>0&&memReq<int64(math.Ceil(float64(in.MemoryRequestsBytes)*.8)){memReq=int64(math.Ceil(float64(in.MemoryRequestsBytes)*.8))}
 confidence:="low";if len(in.CPUSamplesMilli)>=100&&len(in.MemorySamplesBytes)>=100{confidence="medium"}
 return Recommendation{Namespace:in.Namespace,Workload:in.Workload,Container:in.Container,CPURequestMilli:cpuReq,MemoryRequestBytes:memReq,CurrentCPURequestMilli:in.CPURequestsMilli,CurrentMemoryRequestBytes:in.MemoryRequestsBytes,Confidence:confidence,Reason:"P95 CPU x1.30; P99 memory x1.25; minimum floors; max 20% reduction per cycle. Verify peak coverage, OOM history and SLOs before applying.",Applied:false},nil
}
