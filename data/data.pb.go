// Code generated by protoc-gen-go.
// source: data.proto
// DO NOT EDIT!

/*
Package data is a generated protocol buffer package.

It is generated from these files:
	data.proto

It has these top-level messages:
	Event
	MetaData
	ReplicationStart
*/
package data

import proto "github.com/golang/protobuf/proto"
import math "math"

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto.Marshal
var _ = math.Inf

type Event_Type int32

const (
	Event_UPLOAD Event_Type = 1
	Event_DELETE Event_Type = 2
)

var Event_Type_name = map[int32]string{
	1: "UPLOAD",
	2: "DELETE",
}
var Event_Type_value = map[string]int32{
	"UPLOAD": 1,
	"DELETE": 2,
}

func (x Event_Type) Enum() *Event_Type {
	p := new(Event_Type)
	*p = x
	return p
}
func (x Event_Type) String() string {
	return proto.EnumName(Event_Type_name, int32(x))
}
func (x *Event_Type) UnmarshalJSON(data []byte) error {
	value, err := proto.UnmarshalJSONEnum(Event_Type_value, data, "Event_Type")
	if err != nil {
		return err
	}
	*x = Event_Type(value)
	return nil
}

type Event struct {
	Type             *Event_Type `protobuf:"varint,1,req,name=type,enum=data.Event_Type" json:"type,omitempty"`
	Drawer           *string     `protobuf:"bytes,2,req,name=drawer" json:"drawer,omitempty"`
	Filename         *string     `protobuf:"bytes,3,req,name=filename" json:"filename,omitempty"`
	Id               *string     `protobuf:"bytes,4,req,name=id" json:"id,omitempty"`
	XXX_unrecognized []byte      `json:"-"`
}

func (m *Event) Reset()         { *m = Event{} }
func (m *Event) String() string { return proto.CompactTextString(m) }
func (*Event) ProtoMessage()    {}

func (m *Event) GetType() Event_Type {
	if m != nil && m.Type != nil {
		return *m.Type
	}
	return Event_UPLOAD
}

func (m *Event) GetDrawer() string {
	if m != nil && m.Drawer != nil {
		return *m.Drawer
	}
	return ""
}

func (m *Event) GetFilename() string {
	if m != nil && m.Filename != nil {
		return *m.Filename
	}
	return ""
}

func (m *Event) GetId() string {
	if m != nil && m.Id != nil {
		return *m.Id
	}
	return ""
}

type MetaData struct {
	ContentType      *string `protobuf:"bytes,1,req,name=content_type" json:"content_type,omitempty"`
	Source           *string `protobuf:"bytes,2,opt,name=source" json:"source,omitempty"`
	XXX_unrecognized []byte  `json:"-"`
}

func (m *MetaData) Reset()         { *m = MetaData{} }
func (m *MetaData) String() string { return proto.CompactTextString(m) }
func (*MetaData) ProtoMessage()    {}

func (m *MetaData) GetContentType() string {
	if m != nil && m.ContentType != nil {
		return *m.ContentType
	}
	return ""
}

func (m *MetaData) GetSource() string {
	if m != nil && m.Source != nil {
		return *m.Source
	}
	return ""
}

type ReplicationStart struct {
	Event            *string `protobuf:"bytes,1,req,name=event" json:"event,omitempty"`
	XXX_unrecognized []byte  `json:"-"`
}

func (m *ReplicationStart) Reset()         { *m = ReplicationStart{} }
func (m *ReplicationStart) String() string { return proto.CompactTextString(m) }
func (*ReplicationStart) ProtoMessage()    {}

func (m *ReplicationStart) GetEvent() string {
	if m != nil && m.Event != nil {
		return *m.Event
	}
	return ""
}

func init() {
	proto.RegisterEnum("data.Event_Type", Event_Type_name, Event_Type_value)
}
