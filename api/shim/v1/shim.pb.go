// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.36.3
// 	protoc        v3.21.12
// source: shim.proto

package v1

import (
	_go "github.com/prometheus/client_model/go"
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	emptypb "google.golang.org/protobuf/types/known/emptypb"
	reflect "reflect"
	sync "sync"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

type ContainerPhase int32

const (
	ContainerPhase_SCALED_DOWN ContainerPhase = 0
	ContainerPhase_RUNNING     ContainerPhase = 1
)

// Enum value maps for ContainerPhase.
var (
	ContainerPhase_name = map[int32]string{
		0: "SCALED_DOWN",
		1: "RUNNING",
	}
	ContainerPhase_value = map[string]int32{
		"SCALED_DOWN": 0,
		"RUNNING":     1,
	}
)

func (x ContainerPhase) Enum() *ContainerPhase {
	p := new(ContainerPhase)
	*p = x
	return p
}

func (x ContainerPhase) String() string {
	return protoimpl.X.EnumStringOf(x.Descriptor(), protoreflect.EnumNumber(x))
}

func (ContainerPhase) Descriptor() protoreflect.EnumDescriptor {
	return file_shim_proto_enumTypes[0].Descriptor()
}

func (ContainerPhase) Type() protoreflect.EnumType {
	return &file_shim_proto_enumTypes[0]
}

func (x ContainerPhase) Number() protoreflect.EnumNumber {
	return protoreflect.EnumNumber(x)
}

// Deprecated: Use ContainerPhase.Descriptor instead.
func (ContainerPhase) EnumDescriptor() ([]byte, []int) {
	return file_shim_proto_rawDescGZIP(), []int{0}
}

type MetricsRequest struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Empty         *emptypb.Empty         `protobuf:"bytes,1,opt,name=empty,proto3" json:"empty,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *MetricsRequest) Reset() {
	*x = MetricsRequest{}
	mi := &file_shim_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *MetricsRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*MetricsRequest) ProtoMessage() {}

func (x *MetricsRequest) ProtoReflect() protoreflect.Message {
	mi := &file_shim_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use MetricsRequest.ProtoReflect.Descriptor instead.
func (*MetricsRequest) Descriptor() ([]byte, []int) {
	return file_shim_proto_rawDescGZIP(), []int{0}
}

func (x *MetricsRequest) GetEmpty() *emptypb.Empty {
	if x != nil {
		return x.Empty
	}
	return nil
}

type SubscribeStatusRequest struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Empty         *emptypb.Empty         `protobuf:"bytes,1,opt,name=empty,proto3" json:"empty,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *SubscribeStatusRequest) Reset() {
	*x = SubscribeStatusRequest{}
	mi := &file_shim_proto_msgTypes[1]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *SubscribeStatusRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*SubscribeStatusRequest) ProtoMessage() {}

func (x *SubscribeStatusRequest) ProtoReflect() protoreflect.Message {
	mi := &file_shim_proto_msgTypes[1]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use SubscribeStatusRequest.ProtoReflect.Descriptor instead.
func (*SubscribeStatusRequest) Descriptor() ([]byte, []int) {
	return file_shim_proto_rawDescGZIP(), []int{1}
}

func (x *SubscribeStatusRequest) GetEmpty() *emptypb.Empty {
	if x != nil {
		return x.Empty
	}
	return nil
}

type MetricsResponse struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Metrics       []*_go.MetricFamily    `protobuf:"bytes,1,rep,name=metrics,proto3" json:"metrics,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *MetricsResponse) Reset() {
	*x = MetricsResponse{}
	mi := &file_shim_proto_msgTypes[2]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *MetricsResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*MetricsResponse) ProtoMessage() {}

func (x *MetricsResponse) ProtoReflect() protoreflect.Message {
	mi := &file_shim_proto_msgTypes[2]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use MetricsResponse.ProtoReflect.Descriptor instead.
func (*MetricsResponse) Descriptor() ([]byte, []int) {
	return file_shim_proto_rawDescGZIP(), []int{2}
}

func (x *MetricsResponse) GetMetrics() []*_go.MetricFamily {
	if x != nil {
		return x.Metrics
	}
	return nil
}

type ContainerRequest struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Id            string                 `protobuf:"bytes,1,opt,name=id,proto3" json:"id,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *ContainerRequest) Reset() {
	*x = ContainerRequest{}
	mi := &file_shim_proto_msgTypes[3]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ContainerRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ContainerRequest) ProtoMessage() {}

func (x *ContainerRequest) ProtoReflect() protoreflect.Message {
	mi := &file_shim_proto_msgTypes[3]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ContainerRequest.ProtoReflect.Descriptor instead.
func (*ContainerRequest) Descriptor() ([]byte, []int) {
	return file_shim_proto_rawDescGZIP(), []int{3}
}

func (x *ContainerRequest) GetId() string {
	if x != nil {
		return x.Id
	}
	return ""
}

type ContainerStatus struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Id            string                 `protobuf:"bytes,1,opt,name=id,proto3" json:"id,omitempty"`
	Name          string                 `protobuf:"bytes,2,opt,name=name,proto3" json:"name,omitempty"`
	PodName       string                 `protobuf:"bytes,3,opt,name=pod_name,json=podName,proto3" json:"pod_name,omitempty"`
	PodNamespace  string                 `protobuf:"bytes,4,opt,name=pod_namespace,json=podNamespace,proto3" json:"pod_namespace,omitempty"`
	Phase         ContainerPhase         `protobuf:"varint,5,opt,name=phase,proto3,enum=zeropod.shim.v1.ContainerPhase" json:"phase,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *ContainerStatus) Reset() {
	*x = ContainerStatus{}
	mi := &file_shim_proto_msgTypes[4]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ContainerStatus) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ContainerStatus) ProtoMessage() {}

func (x *ContainerStatus) ProtoReflect() protoreflect.Message {
	mi := &file_shim_proto_msgTypes[4]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ContainerStatus.ProtoReflect.Descriptor instead.
func (*ContainerStatus) Descriptor() ([]byte, []int) {
	return file_shim_proto_rawDescGZIP(), []int{4}
}

func (x *ContainerStatus) GetId() string {
	if x != nil {
		return x.Id
	}
	return ""
}

func (x *ContainerStatus) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

func (x *ContainerStatus) GetPodName() string {
	if x != nil {
		return x.PodName
	}
	return ""
}

func (x *ContainerStatus) GetPodNamespace() string {
	if x != nil {
		return x.PodNamespace
	}
	return ""
}

func (x *ContainerStatus) GetPhase() ContainerPhase {
	if x != nil {
		return x.Phase
	}
	return ContainerPhase_SCALED_DOWN
}

var File_shim_proto protoreflect.FileDescriptor

var file_shim_proto_rawDesc = []byte{
	0x0a, 0x0a, 0x73, 0x68, 0x69, 0x6d, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12, 0x0f, 0x7a, 0x65,
	0x72, 0x6f, 0x70, 0x6f, 0x64, 0x2e, 0x73, 0x68, 0x69, 0x6d, 0x2e, 0x76, 0x31, 0x1a, 0x1b, 0x67,
	0x6f, 0x6f, 0x67, 0x6c, 0x65, 0x2f, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x62, 0x75, 0x66, 0x2f, 0x65,
	0x6d, 0x70, 0x74, 0x79, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x1a, 0x22, 0x69, 0x6f, 0x2f, 0x70,
	0x72, 0x6f, 0x6d, 0x65, 0x74, 0x68, 0x65, 0x75, 0x73, 0x2f, 0x63, 0x6c, 0x69, 0x65, 0x6e, 0x74,
	0x2f, 0x6d, 0x65, 0x74, 0x72, 0x69, 0x63, 0x73, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x22, 0x3e,
	0x0a, 0x0e, 0x4d, 0x65, 0x74, 0x72, 0x69, 0x63, 0x73, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74,
	0x12, 0x2c, 0x0a, 0x05, 0x65, 0x6d, 0x70, 0x74, 0x79, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0b, 0x32,
	0x16, 0x2e, 0x67, 0x6f, 0x6f, 0x67, 0x6c, 0x65, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x62, 0x75,
	0x66, 0x2e, 0x45, 0x6d, 0x70, 0x74, 0x79, 0x52, 0x05, 0x65, 0x6d, 0x70, 0x74, 0x79, 0x22, 0x46,
	0x0a, 0x16, 0x53, 0x75, 0x62, 0x73, 0x63, 0x72, 0x69, 0x62, 0x65, 0x53, 0x74, 0x61, 0x74, 0x75,
	0x73, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x12, 0x2c, 0x0a, 0x05, 0x65, 0x6d, 0x70, 0x74,
	0x79, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x16, 0x2e, 0x67, 0x6f, 0x6f, 0x67, 0x6c, 0x65,
	0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x62, 0x75, 0x66, 0x2e, 0x45, 0x6d, 0x70, 0x74, 0x79, 0x52,
	0x05, 0x65, 0x6d, 0x70, 0x74, 0x79, 0x22, 0x4f, 0x0a, 0x0f, 0x4d, 0x65, 0x74, 0x72, 0x69, 0x63,
	0x73, 0x52, 0x65, 0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65, 0x12, 0x3c, 0x0a, 0x07, 0x6d, 0x65, 0x74,
	0x72, 0x69, 0x63, 0x73, 0x18, 0x01, 0x20, 0x03, 0x28, 0x0b, 0x32, 0x22, 0x2e, 0x69, 0x6f, 0x2e,
	0x70, 0x72, 0x6f, 0x6d, 0x65, 0x74, 0x68, 0x65, 0x75, 0x73, 0x2e, 0x63, 0x6c, 0x69, 0x65, 0x6e,
	0x74, 0x2e, 0x4d, 0x65, 0x74, 0x72, 0x69, 0x63, 0x46, 0x61, 0x6d, 0x69, 0x6c, 0x79, 0x52, 0x07,
	0x6d, 0x65, 0x74, 0x72, 0x69, 0x63, 0x73, 0x22, 0x22, 0x0a, 0x10, 0x43, 0x6f, 0x6e, 0x74, 0x61,
	0x69, 0x6e, 0x65, 0x72, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x12, 0x0e, 0x0a, 0x02, 0x69,
	0x64, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x02, 0x69, 0x64, 0x22, 0xac, 0x01, 0x0a, 0x0f,
	0x43, 0x6f, 0x6e, 0x74, 0x61, 0x69, 0x6e, 0x65, 0x72, 0x53, 0x74, 0x61, 0x74, 0x75, 0x73, 0x12,
	0x0e, 0x0a, 0x02, 0x69, 0x64, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x02, 0x69, 0x64, 0x12,
	0x12, 0x0a, 0x04, 0x6e, 0x61, 0x6d, 0x65, 0x18, 0x02, 0x20, 0x01, 0x28, 0x09, 0x52, 0x04, 0x6e,
	0x61, 0x6d, 0x65, 0x12, 0x19, 0x0a, 0x08, 0x70, 0x6f, 0x64, 0x5f, 0x6e, 0x61, 0x6d, 0x65, 0x18,
	0x03, 0x20, 0x01, 0x28, 0x09, 0x52, 0x07, 0x70, 0x6f, 0x64, 0x4e, 0x61, 0x6d, 0x65, 0x12, 0x23,
	0x0a, 0x0d, 0x70, 0x6f, 0x64, 0x5f, 0x6e, 0x61, 0x6d, 0x65, 0x73, 0x70, 0x61, 0x63, 0x65, 0x18,
	0x04, 0x20, 0x01, 0x28, 0x09, 0x52, 0x0c, 0x70, 0x6f, 0x64, 0x4e, 0x61, 0x6d, 0x65, 0x73, 0x70,
	0x61, 0x63, 0x65, 0x12, 0x35, 0x0a, 0x05, 0x70, 0x68, 0x61, 0x73, 0x65, 0x18, 0x05, 0x20, 0x01,
	0x28, 0x0e, 0x32, 0x1f, 0x2e, 0x7a, 0x65, 0x72, 0x6f, 0x70, 0x6f, 0x64, 0x2e, 0x73, 0x68, 0x69,
	0x6d, 0x2e, 0x76, 0x31, 0x2e, 0x43, 0x6f, 0x6e, 0x74, 0x61, 0x69, 0x6e, 0x65, 0x72, 0x50, 0x68,
	0x61, 0x73, 0x65, 0x52, 0x05, 0x70, 0x68, 0x61, 0x73, 0x65, 0x2a, 0x2e, 0x0a, 0x0e, 0x43, 0x6f,
	0x6e, 0x74, 0x61, 0x69, 0x6e, 0x65, 0x72, 0x50, 0x68, 0x61, 0x73, 0x65, 0x12, 0x0f, 0x0a, 0x0b,
	0x53, 0x43, 0x41, 0x4c, 0x45, 0x44, 0x5f, 0x44, 0x4f, 0x57, 0x4e, 0x10, 0x00, 0x12, 0x0b, 0x0a,
	0x07, 0x52, 0x55, 0x4e, 0x4e, 0x49, 0x4e, 0x47, 0x10, 0x01, 0x32, 0x86, 0x02, 0x0a, 0x04, 0x53,
	0x68, 0x69, 0x6d, 0x12, 0x4c, 0x0a, 0x07, 0x4d, 0x65, 0x74, 0x72, 0x69, 0x63, 0x73, 0x12, 0x1f,
	0x2e, 0x7a, 0x65, 0x72, 0x6f, 0x70, 0x6f, 0x64, 0x2e, 0x73, 0x68, 0x69, 0x6d, 0x2e, 0x76, 0x31,
	0x2e, 0x4d, 0x65, 0x74, 0x72, 0x69, 0x63, 0x73, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x1a,
	0x20, 0x2e, 0x7a, 0x65, 0x72, 0x6f, 0x70, 0x6f, 0x64, 0x2e, 0x73, 0x68, 0x69, 0x6d, 0x2e, 0x76,
	0x31, 0x2e, 0x4d, 0x65, 0x74, 0x72, 0x69, 0x63, 0x73, 0x52, 0x65, 0x73, 0x70, 0x6f, 0x6e, 0x73,
	0x65, 0x12, 0x50, 0x0a, 0x09, 0x47, 0x65, 0x74, 0x53, 0x74, 0x61, 0x74, 0x75, 0x73, 0x12, 0x21,
	0x2e, 0x7a, 0x65, 0x72, 0x6f, 0x70, 0x6f, 0x64, 0x2e, 0x73, 0x68, 0x69, 0x6d, 0x2e, 0x76, 0x31,
	0x2e, 0x43, 0x6f, 0x6e, 0x74, 0x61, 0x69, 0x6e, 0x65, 0x72, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73,
	0x74, 0x1a, 0x20, 0x2e, 0x7a, 0x65, 0x72, 0x6f, 0x70, 0x6f, 0x64, 0x2e, 0x73, 0x68, 0x69, 0x6d,
	0x2e, 0x76, 0x31, 0x2e, 0x43, 0x6f, 0x6e, 0x74, 0x61, 0x69, 0x6e, 0x65, 0x72, 0x53, 0x74, 0x61,
	0x74, 0x75, 0x73, 0x12, 0x5e, 0x0a, 0x0f, 0x53, 0x75, 0x62, 0x73, 0x63, 0x72, 0x69, 0x62, 0x65,
	0x53, 0x74, 0x61, 0x74, 0x75, 0x73, 0x12, 0x27, 0x2e, 0x7a, 0x65, 0x72, 0x6f, 0x70, 0x6f, 0x64,
	0x2e, 0x73, 0x68, 0x69, 0x6d, 0x2e, 0x76, 0x31, 0x2e, 0x53, 0x75, 0x62, 0x73, 0x63, 0x72, 0x69,
	0x62, 0x65, 0x53, 0x74, 0x61, 0x74, 0x75, 0x73, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x1a,
	0x20, 0x2e, 0x7a, 0x65, 0x72, 0x6f, 0x70, 0x6f, 0x64, 0x2e, 0x73, 0x68, 0x69, 0x6d, 0x2e, 0x76,
	0x31, 0x2e, 0x43, 0x6f, 0x6e, 0x74, 0x61, 0x69, 0x6e, 0x65, 0x72, 0x53, 0x74, 0x61, 0x74, 0x75,
	0x73, 0x30, 0x01, 0x42, 0x2a, 0x5a, 0x28, 0x67, 0x69, 0x74, 0x68, 0x75, 0x62, 0x2e, 0x63, 0x6f,
	0x6d, 0x2f, 0x63, 0x74, 0x72, 0x6f, 0x78, 0x2f, 0x7a, 0x65, 0x72, 0x6f, 0x70, 0x6f, 0x64, 0x2f,
	0x61, 0x70, 0x69, 0x2f, 0x73, 0x68, 0x69, 0x6d, 0x2f, 0x76, 0x31, 0x2f, 0x3b, 0x76, 0x31, 0x62,
	0x06, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x33,
}

var (
	file_shim_proto_rawDescOnce sync.Once
	file_shim_proto_rawDescData = file_shim_proto_rawDesc
)

func file_shim_proto_rawDescGZIP() []byte {
	file_shim_proto_rawDescOnce.Do(func() {
		file_shim_proto_rawDescData = protoimpl.X.CompressGZIP(file_shim_proto_rawDescData)
	})
	return file_shim_proto_rawDescData
}

var file_shim_proto_enumTypes = make([]protoimpl.EnumInfo, 1)
var file_shim_proto_msgTypes = make([]protoimpl.MessageInfo, 5)
var file_shim_proto_goTypes = []any{
	(ContainerPhase)(0),            // 0: zeropod.shim.v1.ContainerPhase
	(*MetricsRequest)(nil),         // 1: zeropod.shim.v1.MetricsRequest
	(*SubscribeStatusRequest)(nil), // 2: zeropod.shim.v1.SubscribeStatusRequest
	(*MetricsResponse)(nil),        // 3: zeropod.shim.v1.MetricsResponse
	(*ContainerRequest)(nil),       // 4: zeropod.shim.v1.ContainerRequest
	(*ContainerStatus)(nil),        // 5: zeropod.shim.v1.ContainerStatus
	(*emptypb.Empty)(nil),          // 6: google.protobuf.Empty
	(*_go.MetricFamily)(nil),       // 7: io.prometheus.client.MetricFamily
}
var file_shim_proto_depIdxs = []int32{
	6, // 0: zeropod.shim.v1.MetricsRequest.empty:type_name -> google.protobuf.Empty
	6, // 1: zeropod.shim.v1.SubscribeStatusRequest.empty:type_name -> google.protobuf.Empty
	7, // 2: zeropod.shim.v1.MetricsResponse.metrics:type_name -> io.prometheus.client.MetricFamily
	0, // 3: zeropod.shim.v1.ContainerStatus.phase:type_name -> zeropod.shim.v1.ContainerPhase
	1, // 4: zeropod.shim.v1.Shim.Metrics:input_type -> zeropod.shim.v1.MetricsRequest
	4, // 5: zeropod.shim.v1.Shim.GetStatus:input_type -> zeropod.shim.v1.ContainerRequest
	2, // 6: zeropod.shim.v1.Shim.SubscribeStatus:input_type -> zeropod.shim.v1.SubscribeStatusRequest
	3, // 7: zeropod.shim.v1.Shim.Metrics:output_type -> zeropod.shim.v1.MetricsResponse
	5, // 8: zeropod.shim.v1.Shim.GetStatus:output_type -> zeropod.shim.v1.ContainerStatus
	5, // 9: zeropod.shim.v1.Shim.SubscribeStatus:output_type -> zeropod.shim.v1.ContainerStatus
	7, // [7:10] is the sub-list for method output_type
	4, // [4:7] is the sub-list for method input_type
	4, // [4:4] is the sub-list for extension type_name
	4, // [4:4] is the sub-list for extension extendee
	0, // [0:4] is the sub-list for field type_name
}

func init() { file_shim_proto_init() }
func file_shim_proto_init() {
	if File_shim_proto != nil {
		return
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: file_shim_proto_rawDesc,
			NumEnums:      1,
			NumMessages:   5,
			NumExtensions: 0,
			NumServices:   1,
		},
		GoTypes:           file_shim_proto_goTypes,
		DependencyIndexes: file_shim_proto_depIdxs,
		EnumInfos:         file_shim_proto_enumTypes,
		MessageInfos:      file_shim_proto_msgTypes,
	}.Build()
	File_shim_proto = out.File
	file_shim_proto_rawDesc = nil
	file_shim_proto_goTypes = nil
	file_shim_proto_depIdxs = nil
}
