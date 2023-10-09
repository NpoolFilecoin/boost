// Code generated by github.com/whyrusleeping/cbor-gen. DO NOT EDIT.

package message1_1

import (
	"fmt"
	"io"
	"math"
	"sort"

	"github.com/ipfs/go-cid"
	cbg "github.com/whyrusleeping/cbor-gen"
	"golang.org/x/xerrors"
)

var _ = xerrors.Errorf
var _ = cid.Undef
var _ = math.E
var _ = sort.Sort

func (t *TransferMessage1_1) MarshalCBOR(w io.Writer) error {
	if t == nil {
		_, err := w.Write(cbg.CborNull)
		return err
	}

	cw := cbg.NewCborWriter(w)

	if _, err := cw.Write([]byte{163}); err != nil {
		return err
	}

	// t.IsRq (bool) (bool)
	if len("IsRq") > cbg.MaxLength {
		return xerrors.Errorf("Value in field \"IsRq\" was too long")
	}

	if err := cw.WriteMajorTypeHeader(cbg.MajTextString, uint64(len("IsRq"))); err != nil {
		return err
	}
	if _, err := cw.WriteString(string("IsRq")); err != nil {
		return err
	}

	if err := cbg.WriteBool(w, t.IsRq); err != nil {
		return err
	}

	// t.Request (message1_1.TransferRequest1_1) (struct)
	if len("Request") > cbg.MaxLength {
		return xerrors.Errorf("Value in field \"Request\" was too long")
	}

	if err := cw.WriteMajorTypeHeader(cbg.MajTextString, uint64(len("Request"))); err != nil {
		return err
	}
	if _, err := cw.WriteString(string("Request")); err != nil {
		return err
	}

	if err := t.Request.MarshalCBOR(cw); err != nil {
		return err
	}

	// t.Response (message1_1.TransferResponse1_1) (struct)
	if len("Response") > cbg.MaxLength {
		return xerrors.Errorf("Value in field \"Response\" was too long")
	}

	if err := cw.WriteMajorTypeHeader(cbg.MajTextString, uint64(len("Response"))); err != nil {
		return err
	}
	if _, err := cw.WriteString(string("Response")); err != nil {
		return err
	}

	if err := t.Response.MarshalCBOR(cw); err != nil {
		return err
	}
	return nil
}

func (t *TransferMessage1_1) UnmarshalCBOR(r io.Reader) (err error) {
	*t = TransferMessage1_1{}

	cr := cbg.NewCborReader(r)

	maj, extra, err := cr.ReadHeader()
	if err != nil {
		return err
	}
	defer func() {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
	}()

	if maj != cbg.MajMap {
		return fmt.Errorf("cbor input should be of type map")
	}

	if extra > cbg.MaxLength {
		return fmt.Errorf("TransferMessage1_1: map struct too large (%d)", extra)
	}

	var name string
	n := extra

	for i := uint64(0); i < n; i++ {

		{
			sval, err := cbg.ReadString(cr)
			if err != nil {
				return err
			}

			name = string(sval)
		}

		switch name {
		// t.IsRq (bool) (bool)
		case "IsRq":

			maj, extra, err = cr.ReadHeader()
			if err != nil {
				return err
			}
			if maj != cbg.MajOther {
				return fmt.Errorf("booleans must be major type 7")
			}
			switch extra {
			case 20:
				t.IsRq = false
			case 21:
				t.IsRq = true
			default:
				return fmt.Errorf("booleans are either major type 7, value 20 or 21 (got %d)", extra)
			}
			// t.Request (message1_1.TransferRequest1_1) (struct)
		case "Request":

			{

				b, err := cr.ReadByte()
				if err != nil {
					return err
				}
				if b != cbg.CborNull[0] {
					if err := cr.UnreadByte(); err != nil {
						return err
					}
					t.Request = new(TransferRequest1_1)
					if err := t.Request.UnmarshalCBOR(cr); err != nil {
						return xerrors.Errorf("unmarshaling t.Request pointer: %w", err)
					}
				}

			}
			// t.Response (message1_1.TransferResponse1_1) (struct)
		case "Response":

			{

				b, err := cr.ReadByte()
				if err != nil {
					return err
				}
				if b != cbg.CborNull[0] {
					if err := cr.UnreadByte(); err != nil {
						return err
					}
					t.Response = new(TransferResponse1_1)
					if err := t.Response.UnmarshalCBOR(cr); err != nil {
						return xerrors.Errorf("unmarshaling t.Response pointer: %w", err)
					}
				}

			}

		default:
			// Field doesn't exist on this type, so ignore it
			cbg.ScanForLinks(r, func(cid.Cid) {})
		}
	}

	return nil
}
