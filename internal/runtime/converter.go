package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"reflect"

	"github.com/Azure/azure-functions-go-worker/azfunc"
	"github.com/Azure/azure-functions-go-worker/internal/logger"
	"github.com/Azure/azure-functions-go-worker/internal/rpc"
	log "github.com/Sirupsen/logrus"
)

//converter transforms to and from protobuf into native types.

type converter struct {
}

func newConverter() converter {
	c := converter{}
	return c
}

//FromProto converts protobuf parameters to golang values
func (c converter) FromProto(req *rpc.InvocationRequest, eventStream rpc.FunctionRpc_EventStreamClient, f *function) ([]reflect.Value, error) {
	args := make(map[string]reflect.Value)

	// iterate through the invocation request input data
	// if the name of the input data is in the function bindings, then attempt to get the typed binding
	for _, input := range req.InputData {
		param, ok := f.in[input.Name]
		if ok {
			r, err := ConvertToTypeValue(param.Type, input.GetData(), req.GetTriggerMetadata())
			if err != nil {
				log.Debugf("cannot transform typed binding: %v", err)
				return nil, err
			}
			log.Debugf("Converted  data: %v to: %s", input.Data.Data, r.Interface())

			args[input.Name] = r
		} else {
			return nil, fmt.Errorf("cannot find input %v in function bindings", input.Name)
		}
	}

	log.Debugf("args map: %v", args)

	params := make([]reflect.Value, len(f.in))
	for _, v := range f.in {
		if v.Type == reflect.TypeOf((azfunc.Context{})) {
			ctx := azfunc.Context{
				Context:      context.Background(),
				FunctionID:   req.FunctionId,
				InvocationID: req.InvocationId,
				Logger:       logger.NewLogger(eventStream, req.InvocationId),
			}
			params[v.Position] = reflect.ValueOf(ctx)
		} else {
			params[v.Position] = args[v.Name]
		}
	}

	return params, nil
}

//ToProto converts Values to grpc protocol results
func (c converter) ToProto(values []reflect.Value, fields map[string]*funcField) ([]*rpc.ParameterBinding, *rpc.TypedData, error) {
	protoData := make([]*rpc.ParameterBinding, len(fields))

	for _, v := range fields {

		b, err := json.Marshal(values[v.Position].Interface())
		if err != nil {
			log.Debugf("failed to marshal, %v:", err)
		}

		protoData[v.Position] = &rpc.ParameterBinding{
			Name: v.Name,
			Data: &rpc.TypedData{
				Data: &rpc.TypedData_Json{
					Json: string(b),
				},
			},
		}
	}

	// If there are named parameters or no parameters at all there is no return value
	if len(fields) > 0 || len(values) == 0 {
		return protoData, nil, nil
	}

	if len(values) > 2 {
		return nil, nil, fmt.Errorf("Expected 1 or 2 anonymous return values, got %d", len(values))
	}

	ret := ""

	b, err := json.Marshal(values[0].Interface())
	ret = string(b)
	if err != nil {
		log.Debugf("failed to marshal, %v:", err)
	}

	log.Debugf("return params and not out params: %s", ret)

	rv := &rpc.TypedData{
		Data: &rpc.TypedData_Json{
			Json: ret,
		},
	}
	return protoData, rv, nil
}

// ConvertToTypeValue returns a native value from protobuf
func ConvertToTypeValue(pt reflect.Type, data *rpc.TypedData, tm map[string]*rpc.TypedData) (reflect.Value, error) {

	var t reflect.Type

	log.Debugf("pt %s", pt)

	if pt.Kind() == reflect.Ptr {
		t = pt.Elem()
	} else {
		t = pt
	}

	pv := reflect.New(t)
	v := pv.Elem()
	c := 0
	log.Debugf("type is %s, metadata has fields: %v", t, tm)
	for i := 0; t.Kind() == reflect.Struct && i < v.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		log.Debugf("Decoding for field: %s and tag: %v", t.Field(i), tag)

		var td *rpc.TypedData

		if tag == "azfuncdata" {
			log.Debugf("Decoding runtime input data")
			td = data
			c++
		} else if _, ok := tm[tag]; ok {
			td = tm[tag]
			log.Debugf("Decoding runtime input metadata %v", td)
			c++
		} else {
			log.Debugf("Tag %v doesnt exist or doesnt match", tag)
			continue
		}
		d, err := decodeProto(td, t.Field(i).Type)

		if err != nil {
			return reflect.Value{}, err
		}
		v.Field(i).Set(d.Convert(t.Field(i).Type))
	}

	if t.Kind() != reflect.Struct || c < t.NumField() {
		log.Debugf("Binding type does not have any tags, decoding directly into the type")
		d, err := decodeProto(data, t)
		if err != nil {
			return reflect.Value{}, err
		}
		if d.Kind() == reflect.Ptr || d.Kind() == reflect.Map {
			return d, nil
		}

		v.Set(d)
	}

	return pv, nil
}

//decodeProto returns a native value from a protobuf value
func decodeProto(d *rpc.TypedData, t reflect.Type) (reflect.Value, error) {
	switch d.Data.(type) {
	case *rpc.TypedData_Json:
		v := reflect.New(t).Interface()
		if err := json.Unmarshal([]byte(d.GetJson()), &v); err != nil {
			return reflect.Value{}, err
		}
		log.Debugf("Converted to type %s and content %v", t, v)
		return reflect.ValueOf(v).Elem(), nil
	case *rpc.TypedData_String_:
		return reflect.ValueOf(d.GetString_()), nil
	case *rpc.TypedData_Http:
		return decodeHTTP(d.GetHttp())
	case *rpc.TypedData_Bytes:
		return reflect.ValueOf(d.GetBytes()), nil
	case *rpc.TypedData_Stream:
		return reflect.ValueOf(d.GetStream()), nil
	default:
	}
	return reflect.Value{}, fmt.Errorf("Cannot decode %v", d.Data)
}

// decodeHTTP returns a native http.Request from a typed data
func decodeHTTP(d *rpc.RpcHttp) (reflect.Value, error) {

	if d == nil {
		return reflect.Value{}, fmt.Errorf("cannot convert nil request")
	}

	var body io.Reader
	if d.RawBody != nil {
		switch d := d.RawBody.Data.(type) {
		case *rpc.TypedData_String_:
			body = ioutil.NopCloser(bytes.NewBufferString(d.String_))
		}
	}

	req, err := http.NewRequest(d.GetMethod(), d.GetUrl(), body)

	if err != nil {
		return reflect.Value{}, err
	}

	for key, value := range d.GetHeaders() {
		req.Header.Set(key, value)
	}

	return reflect.ValueOf(req), nil
}

//ConvertToTimer returns a formatted TimerInput from an rpc.
func ConvertToTimer(d *rpc.TypedData, tm map[string]*rpc.TypedData) (reflect.Value, error) {

	t, ok := d.Data.(*rpc.TypedData_Json)

	if !ok {
		return reflect.Value{}, fmt.Errorf("cannot convert non json timer")
	}

	timer := &azfunc.Timer{}
	if err := json.Unmarshal([]byte(t.Json), &timer); err != nil {
		return reflect.Value{}, fmt.Errorf("cannot unmarshal timer object")
	}

	return reflect.ValueOf(timer), nil
}

// ConvertToEventGridEvent returns an EventGridEvent
func ConvertToEventGridEvent(d *rpc.TypedData, tm map[string]*rpc.TypedData) (reflect.Value, error) {

	t, ok := d.Data.(*rpc.TypedData_Json)

	if !ok {
		return reflect.Value{}, fmt.Errorf("cannot convert non json event grid event input")
	}

	e := &azfunc.EventGridEvent{}
	if err := json.Unmarshal([]byte(t.Json), &e); err != nil {
		return reflect.Value{}, fmt.Errorf("cannot unmarshal event grid event object")
	}

	return reflect.ValueOf(e), nil
}