// Протобук-сообщения VTM/VTDU — ручная реализация wire-формата.
//
// Номера полей — из ezproto/ezvs.go (LE-EZVIZ-VS, LGPL), генерация protobuf
// protoc'ом не нужна: сообщения крошечные.
//
//	StreamInfoReq:      1=streamurl  2=vtmstreamkey  3=useragent
//	                      4=proxytype  5=pdsstring  6=clnversion  7=pdsnum
//	StreamInfoRsp:      1=result  2=datakey  3=streamhead  4=streamssn
//	                      5=vtmstreamkey  6=serverinfo  7=streamurl
//	                      8=srvinfo  9=aesmd5  10=udptransinfo
//	                      11=peerpbkey  12=pdslist(repeated)
//	StreamKeepAliveReq: 1=streamssn  2=raw_metadata
package cloudcam

import (
	"errors"
	"fmt"
)

const (
	wireVarint = 0
	wireBytes  = 2
)

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendTag(b []byte, field, wire int) []byte {
	return appendVarint(b, uint64(field)<<3|uint64(wire))
}

func appendString(b []byte, field int, s string) []byte {
	b = appendTag(b, field, wireBytes)
	b = appendVarint(b, uint64(len(s)))
	return append(b, s...)
}

func appendBytes(b []byte, field int, p []byte) []byte {
	b = appendTag(b, field, wireBytes)
	b = appendVarint(b, uint64(len(p)))
	return append(b, p...)
}

// appendInt32 — proto3 int32 (отрицательные — 10-байтовый varint со знаком).
func appendInt32(b []byte, field int, v int32) []byte {
	b = appendTag(b, field, wireVarint)
	return appendVarint(b, uint64(uint32(v))|uint64(int64(v)<<32))
}

// readVarint — декодирует varint, возвращает остаток.
func readVarint(b []byte) (uint64, []byte, error) {
	var v uint64
	var shift uint
	for i, c := range b {
		if i >= 10 {
			return 0, nil, errors.New("varint too long")
		}
		v |= uint64(c&0x7F) << shift
		if c < 0x80 {
			return v, b[i+1:], nil
		}
		shift += 7
	}
	return 0, nil, errors.New("unterminated varint")
}

// StreamInfoReq — запрос VTM/VTDU «дай мне этот stream url».
type StreamInfoReq struct {
	Streamurl    string
	Vtmstreamkey string
	Useragent    string
	Proxytype    int32
	Clnversion   string
}

// MarshalProto — wire-форма (как в pyezvizapi build_stream_info_request:
// поле 2 только если vtmstreamkey не пуст, поле 4 (proxytype=0) ВСЕГДА).
func (r *StreamInfoReq) MarshalProto() []byte {
	var b []byte
	b = appendString(b, 1, r.Streamurl)
	if r.Vtmstreamkey != "" {
		b = appendString(b, 2, r.Vtmstreamkey)
	}
	b = appendString(b, 3, r.Useragent)
	b = appendInt32(b, 4, r.Proxytype)
	b = appendString(b, 6, r.Clnversion)
	return b
}

// StreamInfoRsp — ответ VTM/VTDU.
type StreamInfoRsp struct {
	Result       int32
	Datakey      int32
	Streamhead   string
	Streamssn    string
	Vtmstreamkey string
	Serverinfo   string
	Streamurl    string
	Srvinfo      string
	Aesmd5       string
	Udptransinfo string
	Peerpbkey    string
}

// ParseStreamInfoRsp — decode wire-формы.
func ParseStreamInfoRsp(data []byte) (*StreamInfoRsp, error) {
	rsp := &StreamInfoRsp{}
	b := data
	for len(b) > 0 {
		tag, rest, err := readVarint(b)
		if err != nil {
			return nil, err
		}
		b = rest
		field, wire := uint32(tag>>3), tag&0x7
		switch wire {
		case wireVarint:
			v, rest, err := readVarint(b)
			if err != nil {
				return nil, err
			}
			b = rest
			switch field {
			case 1:
				rsp.Result = int32(v)
			case 2:
				rsp.Datakey = int32(v)
			}
		case wireBytes:
			n, rest, err := readVarint(b)
			if err != nil {
				return nil, err
			}
			b = rest
			if uint64(len(b)) < n {
				return nil, errors.New("truncated field")
			}
			s := string(b[:n])
			b = b[n:]
			switch field {
			case 3:
				rsp.Streamhead = s
			case 4:
				rsp.Streamssn = s
			case 5:
				rsp.Vtmstreamkey = s
			case 6:
				rsp.Serverinfo = s
			case 7:
				rsp.Streamurl = s
			case 8:
				rsp.Srvinfo = s
			case 9:
				rsp.Aesmd5 = s
			case 10:
				rsp.Udptransinfo = s
			case 11:
				rsp.Peerpbkey = s
			}
		default:
			return nil, fmt.Errorf("unsupported wire type %d for field %d", wire, field)
		}
	}
	return rsp, nil
}

// StreamKeepAliveReq — keepalive VTDU.
type StreamKeepAliveReq struct {
	Streamssn []byte
}

// MarshalProto — wire-форма.
func (r *StreamKeepAliveReq) MarshalProto() []byte {
	b := appendBytes(nil, 1, r.Streamssn)
	return b
}
