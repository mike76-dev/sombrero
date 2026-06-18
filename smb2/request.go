package smb2

import (
	"crypto/rand"
	"encoding/binary"
	"errors"

	"github.com/mike76-dev/sombrero/utils"
)

var (
	ErrRelatedRequests   = errors.New("first request in a chain cannot be related")
	ErrMixedRequests     = errors.New("wrong usage of the related flag")
	ErrUnalignedRequests = errors.New("chained requests must be aligned to a 8-byte boundary")
	ErrWrongSecurity     = errors.New("request is neither signed nor encrypted")
)

const (
	SMB2CancelRequestMinSize       = 4
	SMB2CancelRequestStructureSize = 4

	SMB2EchoRequestMinSize       = 4
	SMB2EchoRequestStructureSize = 4

	SMB2EchoResponseMinSize       = 4
	SMB2EchoResponseStructureSize = 4
)

// GenericRequest implements a few common methods of SMB2 requests.
type GenericRequest interface {
	Validate(bool) error
	Header() Header
	Len() int
	CancelRequestID() uint64
	GroupID() uint64
	OpenID() []byte
	IsEncrypted() bool
	TransformSessionID() uint64
	CompressReply() bool
}

// Request is the representation of an SMB2 request.
type Request struct {
	cancelRequestID    uint64
	groupID            uint64
	data               []byte
	openID             []byte
	isEncrypted        bool
	transformSessionID uint64
	compressReply      bool
}

// structureSize returns the StructureSize field of an SMB2 request.
func (req Request) structureSize() uint16 {
	if Header(req.data).IsSmb() {
		return 0
	}

	return binary.LittleEndian.Uint16(req.data[SMB2HeaderSize : SMB2HeaderSize+2])
}

// GetRequests parses the message body for SMB/SMB2 requests.
func GetRequests(data []byte, cid uint64, tsid uint64, compressed bool) (reqs []*Request, err error) {
	req := &Request{
		cancelRequestID:    cid,
		data:               data,
		isEncrypted:        tsid != 0,
		transformSessionID: tsid,
		compressReply:      compressed,
	}

	if err := req.Header().Validate(); err != nil {
		return nil, err
	}

	next := req.Header().NextCommand()
	if next == 0 { // A single request
		return []*Request{req}, nil
	}

	var off uint32
	var related bool
	for {
		if next&7 > 0 { // Chained requests must be aligned to a 8-byte boundary
			return nil, ErrUnalignedRequests
		}

		if off == 0 && req.Header().IsFlagSet(FLAGS_RELATED_OPERATIONS) {
			return nil, ErrRelatedRequests
		}

		if len(reqs) > 1 && ((!related && req.Header().IsFlagSet(FLAGS_RELATED_OPERATIONS)) || (related && !req.Header().IsFlagSet(FLAGS_RELATED_OPERATIONS))) {
			return nil, ErrMixedRequests
		}

		if req.Header().IsFlagSet(FLAGS_RELATED_OPERATIONS) { // Related requests
			related = true
		}

		if next > 0 {
			req.data = data[off : off+next]
		}

		reqs = append(reqs, req)
		if next == 0 {
			break
		}

		off += next
		req = &Request{
			cancelRequestID:    cid,
			data:               data[off:],
			isEncrypted:        tsid != 0,
			transformSessionID: tsid,
			compressReply:      compressed,
		}

		if err := req.Header().Validate(); err != nil {
			return nil, err
		}

		next = req.Header().NextCommand()
	}

	// Assign a group ID to all related requests.
	if related {
		gid := make([]byte, 8)
		rand.Read(gid)
		groupID := binary.LittleEndian.Uint64(gid)
		for _, req := range reqs {
			req.groupID = groupID
		}
	}

	return
}

// Validate returns an error if the request is malformed, nil otherwise.
func (req Request) Validate(_ bool) error {
	return req.Header().Validate()
}

// Header casts the request to the Header type.
func (req Request) Header() Header {
	return Header(req.data)
}

// Len returns the length of the request body.
func (req Request) Len() int {
	return len(req.data)
}

// CancelRequestID returns the cancel ID of the request.
func (req Request) CancelRequestID() uint64 {
	return req.cancelRequestID
}

// GroupID returns the group ID of the request.
func (req Request) GroupID() uint64 {
	return req.groupID
}

// OpenID returns the ID of the open associated with the request.
func (req Request) OpenID() []byte {
	return req.openID
}

// SetOpenID associates the request with an open.
func (req *Request) SetOpenID(id []byte) {
	req.openID = make([]byte, 16)
	copy(req.openID, id)
}

// IsEncrypted returns true if the request is encrypted.
func (req Request) IsEncrypted() bool {
	return req.isEncrypted
}

// SetEncrypted changes the encryption status of the request.
func (req *Request) SetEncrypted(enc bool) {
	req.isEncrypted = enc
}

// CompressReply returns true if the response to this request is eligible
// for compression.
func (req Request) CompressReply() bool {
	return req.compressReply
}

// SetCompressReply changes the response compression eligibility of the request.
func (req *Request) SetCompressReply(cr bool) {
	req.compressReply = cr
}

// TransformSessionID returns the SessionID sent by the client in the
// SMB2 TRANSFORM_HEADER, if the request is encrypted.
func (req Request) TransformSessionID() uint64 {
	return req.transformSessionID
}

// SetTransformSessionID sets the TransformSessionID field of the request.
func (req *Request) SetTransformSessionID(id uint64) {
	req.transformSessionID = id
}

// GenericResponse implements a few common methods of SMB2 responses.
type GenericResponse interface {
	FromRequest(req GenericRequest)
	EncodedLength() int
	Encode() []byte
	Header() Header
	GroupID() uint64
	SessionID() uint64
	SetSessionID(id uint64)
	TreeID() uint32
	SetTreeID(id uint32)
	OpenID() []byte
	SetOpenID(id []byte)
	Append(newResp GenericResponse)
	ShouldEncrypt() bool
	MayCompress() bool
}

// Response is the representation of an SMB2 response.
type Response struct {
	data          []byte
	groupID       uint64
	sessionID     uint64
	treeID        uint32
	openID        []byte
	shouldEncrypt bool
	mayCompress   bool
}

// EncodedLength returns the length of the response body.
func (resp Response) EncodedLength() int {
	return len(resp.data)
}

// Encode returns the body of the response.
func (resp Response) Encode() []byte {
	return resp.data
}

// Header casts the response to the Header type.
func (resp Response) Header() Header {
	return Header(resp.data)
}

// GroupID returns the group ID of the response.
func (resp Response) GroupID() uint64 {
	return resp.groupID
}

// SessionID returns the ID of the session associated with the response.
func (resp Response) SessionID() uint64 {
	return resp.sessionID
}

// SetSessionID assigns the response to a session.
func (resp *Response) SetSessionID(id uint64) {
	resp.sessionID = id
}

// TreeID returns the ID of the tree associated with the response.
func (resp Response) TreeID() uint32 {
	return resp.treeID
}

// SetTreeID associates the response with a tree.
func (resp *Response) SetTreeID(id uint32) {
	resp.treeID = id
}

// OpenID returns the ID of the open associated with the response.
func (resp Response) OpenID() []byte {
	return resp.openID
}

// SetOpenID associates the response with an open.
func (resp *Response) SetOpenID(id []byte) {
	resp.openID = make([]byte, 16)
	copy(resp.openID, id)
}

// FromRequest copies the request header into the response and sets the required flags.
func (resp *Response) FromRequest(req GenericRequest) {
	resp.data = make([]byte, SMB2HeaderSize)
	Header(resp.data).CopyFrom(req.Header())
	Header(resp.data).SetStatus(STATUS_OK)
	Header(resp.data).SetFlag(FLAGS_SERVER_TO_REDIR)
	if req.GroupID() == 0 {
		Header(resp.data).ClearFlag(FLAGS_RELATED_OPERATIONS)
	}
	resp.groupID = req.GroupID()
	resp.shouldEncrypt = req.IsEncrypted()
	resp.mayCompress = req.CompressReply()
}

// Append adds a new response to the chain.
func (resp *Response) Append(newResp GenericResponse) {
	var off uint32
	for {
		next := binary.LittleEndian.Uint32(resp.data[off+20 : off+24])
		if next == 0 {
			break
		}
		off += next
	}

	newLen := utils.Roundup(len(resp.data), 8)
	if newLen > len(resp.data) {
		buf := make([]byte, newLen-len(resp.data))
		resp.data = append(resp.data, buf...)
	}

	binary.LittleEndian.PutUint32(resp.data[off+20:off+24], uint32(newLen)-off)
	newResp.Header().SetFlag(FLAGS_RELATED_OPERATIONS)
	resp.data = append(resp.data, newResp.Encode()...)
}

// ShouldEncrypt returns true if the response should be encrypted.
func (resp Response) ShouldEncrypt() bool {
	return resp.shouldEncrypt
}

// MayCompress returns true if the response may be compressed before sending.
func (resp Response) MayCompress() bool {
	return resp.mayCompress
}

// CancelRequest represents an SMB2_CANCEL request.
type CancelRequest struct {
	Request
}

// Validate implements GenericRequest interface.
func (cr CancelRequest) Validate(_ bool) error {
	if err := Header(cr.data).Validate(); err != nil {
		return err
	}

	if len(cr.data) < SMB2HeaderSize+SMB2CancelRequestMinSize {
		return ErrWrongLength
	}

	if cr.structureSize() != SMB2CancelRequestStructureSize {
		return ErrWrongFormat
	}

	return nil
}

// EchoRequest represents an SMB2_ECHO request.
type EchoRequest struct {
	Request
}

// Validate implements GenericRequest interface.
func (er EchoRequest) Validate(_ bool) error {
	if err := Header(er.data).Validate(); err != nil {
		return err
	}

	if len(er.data) < SMB2HeaderSize+SMB2EchoRequestMinSize {
		return ErrWrongLength
	}

	if er.structureSize() != SMB2EchoRequestStructureSize {
		return ErrWrongFormat
	}

	return nil
}

// EchoResponse represents an SMB2_ECHO response.
type EchoResponse struct {
	Response
}

// setStructureSize sets the StructureSize field of the SMB2_ECHO response.
func (er *EchoResponse) setStructureSize() {
	binary.LittleEndian.PutUint16(er.data[SMB2HeaderSize:SMB2HeaderSize+2], SMB2EchoResponseStructureSize)
}

// FromRequest implements GenericResponse interface.
func (er *EchoResponse) FromRequest(req GenericRequest) {
	er.Response.FromRequest(req)

	body := make([]byte, SMB2EchoResponseMinSize)
	er.data = append(er.data, body...)

	er.setStructureSize()
	Header(er.data).SetNextCommand(0)
	Header(er.data).SetStatus(STATUS_OK)
	if Header(er.data).IsFlagSet(FLAGS_ASYNC_COMMAND) {
		Header(er.data).SetCreditResponse(0)
	}
}
