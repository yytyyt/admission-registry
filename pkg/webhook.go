package pkg

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	codev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/klog"
	"net/http"
	"strings"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecFactory  = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecFactory.UniversalDeserializer()
)

const (
	AnnotationMutateKey = "io.ydzs.admission-registry/mutate"
	AnnotationStatusKey = "io.ydzs.admission-registry/status"
)

type WhSrvParam struct {
	Port     int
	CertFile string
	KeyFile  string
}

type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

type WebhookServer struct {
	Server              *http.Server // http server
	WhiteListRegistries []string     // 白名单的镜像仓库列表
}

func (s *WebhookServer) Handler(writer http.ResponseWriter, request *http.Request) {
	var body []byte
	if request.Body != nil {
		if data, err := ioutil.ReadAll(request.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		klog.Error("empty data body")
		http.Error(writer, "empty data body", http.StatusBadRequest)
		return
	}
	// 校验 content-type
	contentType := request.Header.Get("Content-Type")
	if contentType != "application/json" {
		klog.Errorf("Content-Type is %s,but except application/json", contentType)
		http.Error(writer, "Content-Type invalid,except application/json", http.StatusBadRequest)
	}
	// 数据序列化 (validate、mutate)请求的数据都是 AdmissionReview
	var admissionResponse *admissionv1.AdmissionResponse

	requestAdmissionReview := admissionv1.AdmissionReview{}

	_, _, err := deserializer.Decode(body, nil, &requestAdmissionReview)
	if err != nil {
		klog.Errorf("can not decode body:%v", err)
		admissionResponse = &admissionv1.AdmissionResponse{
			Allowed: false,
			Result: &metav1.Status{
				Message: err.Error(),
				Code:    http.StatusInternalServerError,
			},
		}
	} else {
		// 序列化成功 获取到了请求的 AdmissionReview 数据
		if request.URL.Path == "/mutate" {
			admissionResponse = s.mutate(&requestAdmissionReview)
		} else if request.URL.Path == "/validate" {
			admissionResponse = s.validate(&requestAdmissionReview)
		}
	}

	// 构造返回的 AdmissionReview 结构体
	responseAdmissionReview := admissionv1.AdmissionReview{}
	responseAdmissionReview.APIVersion = requestAdmissionReview.APIVersion
	responseAdmissionReview.Kind = requestAdmissionReview.Kind

	if admissionResponse != nil {
		responseAdmissionReview.Response = admissionResponse
		if requestAdmissionReview.Request != nil { // 要返回相同的 UID
			responseAdmissionReview.Response.UID = requestAdmissionReview.Request.UID
		}
	}

	klog.Errorf(fmt.Sprintf("sending response:%v", responseAdmissionReview.Response))
	// send response
	respBytes, err := json.Marshal(responseAdmissionReview)
	if err != nil {
		klog.Errorf("Can not encode response:%v", err)
		http.Error(writer, fmt.Sprintf("Can not encode response:%v", err), http.StatusBadRequest)
		return
	}
	klog.Info("Read to write response...")
	if _, err = writer.Write(respBytes); err != nil {
		klog.Errorf("Can not write response:%v", err)
		http.Error(writer, fmt.Sprintf("Can not write response:%v", err), http.StatusBadRequest)
	}

}

func (s *WebhookServer) validate(ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	req := ar.Request
	var (
		allowed = true
		code    = http.StatusOK
		message = ""
	)
	klog.Info("AdmissionReview for Kind=%s,Namespace=%s,name=%s,UID=%s", req.Kind.Kind, req.Namespace, req.Name, req.UID)

	var pod codev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		klog.Errorf("Can not unmarshal object raw:%v", err)
		allowed = false
		code = http.StatusBadRequest
		message = err.Error()
	}
	// 处理真正的业务逻辑
	for _, container := range pod.Spec.Containers {
		var whitelisted = false
		for _, registry := range s.WhiteListRegistries {
			if strings.HasPrefix(container.Image, registry) {
				whitelisted = true
			}
		}
		if !whitelisted {
			allowed = false
			code = http.StatusForbidden
			message = fmt.Sprintf("%s image comes from an untrusted registry! Only images from %v are allowd ", container.Image, s.WhiteListRegistries)
			break
		}
	}
	return &admissionv1.AdmissionResponse{
		Allowed: allowed,
		Result: &metav1.Status{
			Code:    int32(code),
			Message: message,
		},
	}
}

func (s *WebhookServer) mutate(ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	// Deployment、Service -> Annotations: AnnotationMutateKey、AnnotationStatusKey
	req := ar.Request
	var (
		objectMeta *metav1.ObjectMeta
	)

	klog.Info("AdmissionReview for Kind=%s,Namespace=%s,name=%s,UID=%s", req.Kind.Kind, req.Namespace, req.Name, req.UID)

	switch req.Kind.Kind {
	case "Deployment":
		var deployment appsv1.Deployment
		if err := json.Unmarshal(req.Object.Raw, &deployment); err != nil {
			klog.Errorf("Can not unmarshal object raw:%v", err)
			return &admissionv1.AdmissionResponse{
				Result: &metav1.Status{
					Code:    http.StatusBadRequest,
					Message: err.Error(),
				},
			}
		}
		objectMeta = &deployment.ObjectMeta

	case "Service":
		var service codev1.Service
		if err := json.Unmarshal(req.Object.Raw, &service); err != nil {
			klog.Errorf("Can not unmarshal object raw:%v", err)
			return &admissionv1.AdmissionResponse{
				Result: &metav1.Status{
					Code:    http.StatusBadRequest,
					Message: err.Error(),
				},
			}
		}
		objectMeta = &service.ObjectMeta
	default:
		return &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Can not handle the kind(%s) object", req.Kind.Kind),
			},
		}
	}
	// 判断是否需要真的执行 mutate 操作
	if !mutationRequired(objectMeta) {
		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}
	}
	// 需要执行 mutate 操作
	annotations := map[string]string{
		AnnotationStatusKey: "mutated",
	}
	var patch []patchOperation
	patch = append(patch, mutateAnnotations(objectMeta.Annotations, annotations)...)
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		klog.Errorf("patch marshal err:%v", err)
		return &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Code:    http.StatusBadRequest,
				Message: err.Error(),
			},
		}
	}

	return &admissionv1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *admissionv1.PatchType {
			pt := admissionv1.PatchTypeJSONPatch
			return &pt
		}(),
	}

}

func mutationRequired(metadata *metav1.ObjectMeta) bool {
	annotations := metadata.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	var required bool

	switch strings.ToLower(annotations[AnnotationMutateKey]) {
	case "n", "no", "false", "off":
		required = false
	default:
		required = true
	}

	status := annotations[AnnotationStatusKey]
	if strings.ToLower(status) == "mutated" {
		required = false
	}
	klog.Info("Mutation policy for %s/%s:required:%v", metadata.Name, metadata.Namespace, required)

	return required
}

func mutateAnnotations(target, added map[string]string) (patch []patchOperation) {

	for key, value := range added {
		if target == nil || target[key] == "" {
			patch = append(patch, patchOperation{
				Op:   "add",
				Path: "/metadata/annotations",
				Value: map[string]string{
					key: value,
				},
			})
		} else {
			patch = append(patch, patchOperation{
				Op:    "replace",
				Path:  "/metadata/annotations/" + key,
				Value: key,
			})
		}
	}
	return
}
