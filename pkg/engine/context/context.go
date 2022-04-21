package context

import (
	"encoding/json"
	"strings"
	"sync"

	jsonpatch "github.com/evanphx/json-patch/v5"
	kyverno "github.com/kyverno/kyverno/api/kyverno/v1"
	pkgcommon "github.com/kyverno/kyverno/pkg/common"
	kubeutils "github.com/kyverno/kyverno/pkg/utils/kube"
	"github.com/pkg/errors"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var logger = log.Log.WithName("context")

// EvalInterface is used to query and inspect context data
type EvalInterface interface {
	// Query accepts a JMESPath expression and returns matching data
	Query(query string) (interface{}, error)

	// HasChanged accepts a JMESPath expression and compares matching data in the
	// request.object and request.oldObject context fields. If the data has changed
	// it return `true`. If the data has not changed it returns false. If either
	// request.object or request.oldObject are not found, an error is returned.
	HasChanged(jmespath string) (bool, error)
}

// Interface to manage context operations
type Interface interface {
	// AddRequest marshals and adds the admission request to the context
	AddRequest(request *admissionv1.AdmissionRequest) error

	// AddVariable adds a variable to the context
	AddVariable(key, value string) error

	// AddContextEntry adds a context entry to the context
	AddContextEntry(name string, dataRaw []byte) error

	// AddResource merges resource json under request.object
	AddResource(data map[string]interface{}) error

	// AddOldResource merges resource json under request.oldObject
	AddOldResource(data map[string]interface{}) error

	// AddUserInfo merges userInfo json under kyverno.userInfo
	AddUserInfo(userInfo kyverno.RequestInfo) error

	// AddServiceAccount merges ServiceAccount types
	AddServiceAccount(userName string) error

	// AddNamespace merges resource json under request.namespace
	AddNamespace(namespace string) error

	// AddElement adds element info to the context
	AddElement(data map[string]interface{}, index int) error

	// AddImageInfo adds image info to the context
	AddImageInfo(info kubeutils.ImageInfo) error

	// AddImageInfos adds image infos to the context
	AddImageInfos(resource *unstructured.Unstructured) error

	// ImageInfo returns image infos present in the context
	ImageInfo() map[string]map[string]kubeutils.ImageInfo

	// GenerateCustomImageInfo returns image infos as defined by a custom image extraction config
	// and updates the context
	GenerateCustomImageInfo(resource *unstructured.Unstructured, imageExtractorConfigs kubeutils.ImageExtractorConfigs) (map[string]map[string]kubeutils.ImageInfo, error)

	// Checkpoint creates a copy of the current internal state and pushes it into a stack of stored states.
	Checkpoint()

	// Restore sets the internal state to the last checkpoint, and removes the checkpoint.
	Restore()

	// Reset sets the internal state to the last checkpoint, but does not remove the checkpoint.
	Reset()

	EvalInterface

	// AddJSON  merges the json with context
	addJSON(dataRaw []byte) error
}

// Context stores the data resources as JSON
type context struct {
	mutex              sync.RWMutex
	jsonRaw            []byte
	jsonRawCheckpoints [][]byte
	images             map[string]map[string]kubeutils.ImageInfo
}

// NewContext returns a new context
func NewContext() Interface {
	return NewContextFromRaw([]byte(`{}`))
}

// NewContextFromRaw returns a new context initialized with raw data
func NewContextFromRaw(raw []byte) Interface {
	ctx := context{
		jsonRaw:            raw,
		jsonRawCheckpoints: make([][]byte, 0),
	}
	return &ctx
}

// addJSON merges json data
func (ctx *context) addJSON(dataRaw []byte) error {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	json, err := jsonpatch.MergeMergePatches(ctx.jsonRaw, dataRaw)
	if err != nil {
		return errors.Wrap(err, "failed to merge JSON data")
	}
	ctx.jsonRaw = json
	return nil
}

// AddRequest adds an admission request to context
func (ctx *context) AddRequest(request *admissionv1.AdmissionRequest) error {
	return addToContext(ctx, request, "request")
}

func (ctx *context) AddVariable(key, value string) error {
	return ctx.addJSON(pkgcommon.VariableToJSON(key, value))
}

func (ctx *context) AddContextEntry(name string, dataRaw []byte) error {
	var data interface{}
	if err := json.Unmarshal(dataRaw, &data); err != nil {
		logger.Error(err, "failed to unmarshal the resource")
		return err
	}
	return addToContext(ctx, data, name)
}

// AddResource data at path: request.object
func (ctx *context) AddResource(data map[string]interface{}) error {
	return addToContext(ctx, data, "request", "object")
}

// AddOldResource data at path: request.oldObject
func (ctx *context) AddOldResource(data map[string]interface{}) error {
	return addToContext(ctx, data, "request", "oldObject")
}

// AddUserInfo adds userInfo at path request.userInfo
func (ctx *context) AddUserInfo(userRequestInfo kyverno.RequestInfo) error {
	return addToContext(ctx, userRequestInfo, "request")
}

// AddServiceAccount removes prefix 'system:serviceaccount:' and namespace, then loads only SA name and SA namespace
func (ctx *context) AddServiceAccount(userName string) error {
	saPrefix := "system:serviceaccount:"
	var sa string
	saName := ""
	saNamespace := ""
	if len(userName) <= len(saPrefix) {
		sa = ""
	} else {
		sa = userName[len(saPrefix):]
	}
	// filter namespace
	groups := strings.Split(sa, ":")
	if len(groups) >= 2 {
		saName = groups[1]
		saNamespace = groups[0]
	}
	saNameObj := struct {
		SA string `json:"serviceAccountName"`
	}{
		SA: saName,
	}
	saNameRaw, err := json.Marshal(saNameObj)
	if err != nil {
		logger.Error(err, "failed to marshal the SA")
		return err
	}
	if err := ctx.addJSON(saNameRaw); err != nil {
		return err
	}

	saNsObj := struct {
		SA string `json:"serviceAccountNamespace"`
	}{
		SA: saNamespace,
	}
	saNsRaw, err := json.Marshal(saNsObj)
	if err != nil {
		logger.Error(err, "failed to marshal the SA namespace")
		return err
	}
	if err := ctx.addJSON(saNsRaw); err != nil {
		return err
	}
	logger.V(4).Info("Adding service account", "service account name", saName, "service account namespace", saNamespace)
	return nil
}

// AddNamespace merges resource json under request.namespace
func (ctx *context) AddNamespace(namespace string) error {
	return addToContext(ctx, namespace, "request", "namespace")
}

func (ctx *context) AddElement(data map[string]interface{}, index int) error {
	data = map[string]interface{}{
		"element":      data,
		"elementIndex": index,
	}
	return addToContext(ctx, data)
}

func (ctx *context) AddImageInfo(info kubeutils.ImageInfo) error {
	data := map[string]interface{}{
		"image":    info.String(),
		"registry": info.Registry,
		"path":     info.Path,
		"name":     info.Name,
		"tag":      info.Tag,
		"digest":   info.Digest,
	}
	return addToContext(ctx, data, "image")
}

func (ctx *context) AddImageInfos(resource *unstructured.Unstructured) error {
	images, err := kubeutils.ExtractImagesFromResource(*resource, nil)
	if err != nil {
		return err
	}
	if len(images) == 0 {
		return nil
	}
	ctx.images = images
	return addToContext(ctx, images, "images")
}

func (ctx *context) GenerateCustomImageInfo(resource *unstructured.Unstructured, imageExtractorConfigs kubeutils.ImageExtractorConfigs) (map[string]map[string]kubeutils.ImageInfo, error) {
	images, err := kubeutils.ExtractImagesFromResource(*resource, imageExtractorConfigs)
	if err != nil {
		return nil, err
	}
	if len(images) == 0 {
		return nil, nil
	}
	return images, addToContext(ctx, images, "images")
}

func (ctx *context) ImageInfo() map[string]map[string]kubeutils.ImageInfo {
	return ctx.images
}

// Checkpoint creates a copy of the current internal state and
// pushes it into a stack of stored states.
func (ctx *context) Checkpoint() {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	jsonRawCheckpoint := make([]byte, len(ctx.jsonRaw))
	copy(jsonRawCheckpoint, ctx.jsonRaw)
	ctx.jsonRawCheckpoints = append(ctx.jsonRawCheckpoints, jsonRawCheckpoint)
}

// Restore sets the internal state to the last checkpoint, and removes the checkpoint.
func (ctx *context) Restore() {
	ctx.reset(true)
}

// Reset sets the internal state to the last checkpoint, but does not remove the checkpoint.
func (ctx *context) Reset() {
	ctx.reset(false)
}

func (ctx *context) reset(remove bool) {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	if len(ctx.jsonRawCheckpoints) == 0 {
		return
	}
	n := len(ctx.jsonRawCheckpoints) - 1
	jsonRawCheckpoint := ctx.jsonRawCheckpoints[n]
	ctx.jsonRaw = make([]byte, len(jsonRawCheckpoint))
	copy(ctx.jsonRaw, jsonRawCheckpoint)
	if remove {
		ctx.jsonRawCheckpoints = ctx.jsonRawCheckpoints[:n]
	}
}