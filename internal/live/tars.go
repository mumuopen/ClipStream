package live

// TARS 协议编解码（虎牙弹幕用，最小实现）
// 血统：腾讯 TARS 编码（tag+type 头，大端值），对齐 tars_dart 的 TarsInputStream/OutputStream。
// 编码要点：
//   - head：1 字节 (tag<<4)|type；tag>=15 时首字节 0xF0|type + 第二字节 (tag-15)
//   - int 为 0 时只写 head（type=14 ZERO，无值字节）
//   - int：type=4（4字节）/ type=5（8字节）大端
//   - bool：type=1 + 1 字节
//   - string：type=8（1字节长）+ type=9（4字节长）
//   - bytes：type=15（4字节长 + 原始数据）
//   - struct：type=12 起始，结束 head（tag=0,type=13）

import (
	"encoding/binary"
	"fmt"
)

const (
	tarsTypeNull     = 0
	tarsTypeBool     = 1
	tarsTypeByte     = 2
	tarsTypeShort    = 3
	tarsTypeInt      = 4
	tarsTypeLong     = 5
	tarsTypeFloat    = 6
	tarsTypeDouble   = 7
	tarsTypeString1  = 8
	tarsTypeString4  = 9
	tarsTypeMap      = 10
	tarsTypeList     = 11
	tarsTypeStructB  = 12
	tarsTypeStructE  = 13
	tarsTypeZero     = 14
	tarsTypeSimpleLs = 15
)

// ── 写端 ──

type tarsWriter struct {
	buf []byte
}

func (w *tarsWriter) head(tag, typ int) {
	if tag < 15 {
		w.buf = append(w.buf, byte(tag<<4|typ))
	} else {
		w.buf = append(w.buf, byte(0xF0|typ), byte(tag-15))
	}
}

func (w *tarsWriter) writeInt(tag int, v int64) {
	if v == 0 {
		w.head(tag, tarsTypeZero)
		return
	}
	if v > -2147483648 && v <= 2147483647 {
		w.head(tag, tarsTypeInt)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(int32(v)))
		w.buf = append(w.buf, b[:]...)
	} else {
		w.head(tag, tarsTypeLong)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(v))
		w.buf = append(w.buf, b[:]...)
	}
}

func (w *tarsWriter) writeBool(tag int, v bool) {
	w.head(tag, tarsTypeBool)
	if v {
		w.buf = append(w.buf, 1)
	} else {
		w.buf = append(w.buf, 0)
	}
}

func (w *tarsWriter) writeString(tag int, s string) {
	n := len(s)
	if n < 256 {
		w.head(tag, tarsTypeString1)
		w.buf = append(w.buf, byte(n))
	} else {
		w.head(tag, tarsTypeString4)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(n))
		w.buf = append(w.buf, b[:]...)
	}
	w.buf = append(w.buf, s...)
}

func (w *tarsWriter) writeBytes(tag int, b []byte) {
	w.head(tag, tarsTypeSimpleLs)
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(b)))
	w.buf = append(w.buf, l[:]...)
	w.buf = append(w.buf, b...)
}

func (w *tarsWriter) writeStructBegin(tag int) {
	w.head(tag, tarsTypeStructB)
}

func (w *tarsWriter) writeStructEnd() {
	w.buf = append(w.buf, byte(tarsTypeStructE)) // head: tag=0, type=13
}

// writeStringMap 编码 string→string map（type=10：4B count + 每项 key/value 各带 head）
func (w *tarsWriter) writeStringMap(tag int, m map[string]string) {
	w.head(tag, tarsTypeMap)
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(m)))
	w.buf = append(w.buf, l[:]...)
	for k, v := range m {
		w.writeString(0, k)
		w.writeString(0, v)
	}
}

func (w *tarsWriter) bytes() []byte { return w.buf }

// ── 读端 ──

type tarsReader struct {
	buf []byte
	pos int
}

func newTarsReader(b []byte) *tarsReader { return &tarsReader{buf: b} }

func (r *tarsReader) readHead() (tag, typ int, err error) {
	if r.pos >= len(r.buf) {
		return 0, 0, fmt.Errorf("tars 越界")
	}
	b := r.buf[r.pos]
	r.pos++
	tag = int(b >> 4)
	typ = int(b & 0x0F)
	if tag == 15 {
		if r.pos >= len(r.buf) {
			return 0, 0, fmt.Errorf("tars head 越界")
		}
		tag = 15 + int(r.buf[r.pos])
		r.pos++
	}
	return tag, typ, nil
}

// readInt 读取按当前 head 已消费后的值（调用方需先 readHead 取 typ）
func (r *tarsReader) readInt(typ int) int64 {
	switch typ {
	case tarsTypeZero:
		return 0
	case tarsTypeBool, tarsTypeByte:
		v := int64(r.buf[r.pos])
		r.pos++
		return v
	case tarsTypeShort:
		v := int64(int16(binary.BigEndian.Uint16(r.buf[r.pos:])))
		r.pos += 2
		return v
	case tarsTypeInt:
		v := int64(int32(binary.BigEndian.Uint32(r.buf[r.pos:])))
		r.pos += 4
		return v
	case tarsTypeLong:
		v := int64(binary.BigEndian.Uint64(r.buf[r.pos:]))
		r.pos += 8
		return v
	}
	return 0
}

func (r *tarsReader) readBytes(typ int) []byte {
	switch typ {
	case tarsTypeString1:
		n := int(r.buf[r.pos])
		r.pos++
		v := r.buf[r.pos : r.pos+n]
		r.pos += n
		return v
	case tarsTypeString4, tarsTypeSimpleLs:
		n := int(binary.BigEndian.Uint32(r.buf[r.pos:]))
		r.pos += 4
		v := r.buf[r.pos : r.pos+n]
		r.pos += n
		return v
	}
	return nil
}

// skip 跳过未知字段（按 type）
func (r *tarsReader) skip(typ int) {
	switch typ {
	case tarsTypeZero, tarsTypeStructE:
		return
	case tarsTypeBool, tarsTypeByte:
		r.pos++
	case tarsTypeShort:
		r.pos += 2
	case tarsTypeInt, tarsTypeFloat:
		r.pos += 4
	case tarsTypeLong, tarsTypeDouble:
		r.pos += 8
	case tarsTypeString1:
		n := int(r.buf[r.pos])
		r.pos += 1 + n
	case tarsTypeString4, tarsTypeSimpleLs:
		n := int(binary.BigEndian.Uint32(r.buf[r.pos:]))
		r.pos += 4 + n
	case tarsTypeStructB:
		// 递归跳过结构体直到结束标记
		for {
			_, t2, err := r.readHead()
			if err != nil {
				return
			}
			if t2 == tarsTypeStructE {
				return
			}
			r.skip(t2)
		}
	default:
		// map/list 等复杂类型无法跳过，直接放弃剩余
		r.pos = len(r.buf)
	}
}