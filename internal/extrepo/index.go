package extrepo

import (
	"errors"
)

// Decodificador mínimo del índice de Keiyoushi (index.pb, protobuf). Esquema:
// https://github.com/keiyoushi/extensions-source/blob/main/.github/scripts/index.proto
// Se lee el formato de cable a mano para no arrastrar la biblioteca de protobuf.

type Index struct {
	Name       string
	SigningKey string
	Extensions []IndexExt
}

type IndexExt struct {
	Name, Pkg, JarURL, ApkURL, IconURL, VersionName, Lib string
	VersionCode                                          int64
	ContentWarning                                       int
	Sources                                              []IndexSource
}

type IndexSource struct {
	ID      int64  `json:"id,string"`
	Name    string `json:"name"`
	Lang    string `json:"lang"`
	HomeURL string `json:"homeUrl"`
}

type pbField struct {
	num  int
	wire int
	val  uint64
	buf  []byte
}

func pbRead(b []byte, fn func(pbField) error) error {
	for len(b) > 0 {
		tag, n := uvarint(b)
		if n <= 0 {
			return errors.New("protobuf: varint inválido")
		}
		b = b[n:]
		f := pbField{num: int(tag >> 3), wire: int(tag & 7)}
		switch f.wire {
		case 0:
			v, n := uvarint(b)
			if n <= 0 {
				return errors.New("protobuf: varint inválido")
			}
			f.val, b = v, b[n:]
		case 1:
			if len(b) < 8 {
				return errors.New("protobuf: truncado")
			}
			b = b[8:]
		case 2:
			l, n := uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return errors.New("protobuf: longitud inválida")
			}
			f.buf, b = b[n:n+int(l)], b[n+int(l):]
		case 5:
			if len(b) < 4 {
				return errors.New("protobuf: truncado")
			}
			b = b[4:]
		default:
			return errors.New("protobuf: tipo de cable no soportado")
		}
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}

func uvarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, c := range b {
		if i == 10 {
			return 0, -1
		}
		if c < 0x80 {
			return x | uint64(c)<<s, i + 1
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, 0
}

func ParseIndex(b []byte) (*Index, error) {
	idx := &Index{}
	err := pbRead(b, func(f pbField) error {
		switch f.num {
		case 1:
			idx.Name = string(f.buf)
		case 3:
			idx.SigningKey = string(f.buf)
		case 101: // ExtensionList
			return pbRead(f.buf, func(f pbField) error {
				if f.num != 1 {
					return nil
				}
				var e IndexExt
				err := pbRead(f.buf, func(f pbField) error {
					switch f.num {
					case 1:
						e.Name = string(f.buf)
					case 2:
						e.Pkg = string(f.buf)
					case 3:
						return pbRead(f.buf, func(f pbField) error {
							switch f.num {
							case 1:
								e.ApkURL = string(f.buf)
							case 2:
								e.IconURL = string(f.buf)
							case 501:
								e.JarURL = string(f.buf)
							}
							return nil
						})
					case 4:
						e.Lib = string(f.buf)
					case 5:
						e.VersionCode = int64(f.val)
					case 6:
						e.VersionName = string(f.buf)
					case 7:
						e.ContentWarning = int(f.val)
					case 8:
						var s IndexSource
						if err := pbRead(f.buf, func(f pbField) error {
							switch f.num {
							case 1:
								s.ID = int64(f.val)
							case 2:
								s.Name = string(f.buf)
							case 3:
								s.Lang = string(f.buf)
							case 4:
								s.HomeURL = string(f.buf)
							}
							return nil
						}); err != nil {
							return err
						}
						e.Sources = append(e.Sources, s)
					}
					return nil
				})
				if err != nil {
					return err
				}
				idx.Extensions = append(idx.Extensions, e)
				return nil
			})
		case 102:
			return errors.New("índice con extensionListUrl: no soportado")
		}
		return nil
	})
	return idx, err
}
