//                           _       _
// __      _____  __ ___   ___  __ _| |_ ___
// \ \ /\ / / _ \/ _` \ \ / / |/ _` | __/ _ \
//  \ V  V /  __/ (_| |\ V /| | (_| | ||  __/
//   \_/\_/ \___|\__,_| \_/ |_|\__,_|\__\___|
//
//  Copyright © 2016 - 2024 Weaviate B.V. All rights reserved.
//
//  CONTACT: hello@weaviate.io
//

package storobj

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"runtime"

	"github.com/buger/jsonparser"
	"github.com/go-openapi/strfmt"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/vmihailenco/msgpack/v5"
	"github.com/weaviate/weaviate/entities/additional"
	errwrap "github.com/weaviate/weaviate/entities/errors"
	"github.com/weaviate/weaviate/entities/models"
	"github.com/weaviate/weaviate/entities/schema"
	"github.com/weaviate/weaviate/entities/search"
	"github.com/weaviate/weaviate/usecases/byteops"
)

var bufPool *bufferPool

type Vectors map[string][]float32

func init() {
	// a 10kB buffer should be large enough for typical cases, it can fit a
	// 1536d uncompressed vector and about 3kB of object payload. If the
	// initial size is not large enoug, the caller can always allocate a larger
	// buffer and return that to the pool instead.
	bufPool = newBufferPool(10 * 1024)
}

type Object struct {
	MarshallerVersion uint8
	Object            models.Object `json:"object"`
	Vector            []float32     `json:"vector"`
	VectorLen         int           `json:"-"`
	BelongsToNode     string        `json:"-"`
	BelongsToShard    string        `json:"-"`
	IsConsistent      bool          `json:"-"`
	DocID             uint64
	Vectors           map[string][]float32 `json:"vectors"`
}

func New(docID uint64) *Object {
	return &Object{
		MarshallerVersion: 1,
		DocID:             docID,
	}
}

func FromObject(object *models.Object, vector []float32, vectors models.Vectors) *Object {
	// clear out nil entries of properties to make sure leaving a property out and setting it nil is identical
	properties, ok := object.Properties.(map[string]interface{})
	if ok {
		for key, prop := range properties {
			if prop == nil {
				delete(properties, key)
			}
		}
		object.Properties = properties
	}

	var vecs map[string][]float32
	if vectors != nil {
		vecs = make(map[string][]float32)
		for targetVector, vector := range vectors {
			vecs[targetVector] = vector
		}
	}

	return &Object{
		Object:            *object,
		Vector:            vector,
		MarshallerVersion: 1,
		VectorLen:         len(vector),
		Vectors:           vecs,
	}
}

func FromBinary(data []byte) (*Object, error) {
	ko := &Object{}
	if err := ko.UnmarshalBinary(data); err != nil {
		return nil, err
	}

	return ko, nil
}

func FromBinaryUUIDOnly(data []byte) (*Object, error) {
	ko := &Object{}

	rw := byteops.NewReadWriter(data)
	version := rw.ReadUint8()
	if version != 1 {
		return nil, errors.Errorf("unsupported binary marshaller version %d", version)
	}

	ko.MarshallerVersion = version

	ko.DocID = rw.ReadUint64()
	rw.MoveBufferPositionForward(1) // ignore kind-byte
	uuidObj, err := uuid.FromBytes(rw.ReadBytesFromBuffer(16))
	if err != nil {
		return nil, fmt.Errorf("parse uuid: %w", err)
	}
	ko.Object.ID = strfmt.UUID(uuidObj.String())

	rw.MoveBufferPositionForward(16)

	vecLen := rw.ReadUint16()
	rw.MoveBufferPositionForward(uint64(vecLen * 4))
	classNameLen := rw.ReadUint16()

	ko.Object.Class = string(rw.ReadBytesFromBuffer(uint64(classNameLen)))

	return ko, nil
}

func FromBinaryOptional(data []byte,
	addProp additional.Properties, properties *PropertyExtraction,
) (*Object, error) {
	ko := &Object{}

	rw := byteops.NewReadWriter(data)
	ko.MarshallerVersion = rw.ReadUint8()
	if ko.MarshallerVersion != 1 {
		return nil, errors.Errorf("unsupported binary marshaller version %d", ko.MarshallerVersion)
	}
	ko.DocID = rw.ReadUint64()
	rw.MoveBufferPositionForward(1) // ignore kind-byte
	uuidObj, err := uuid.FromBytes(rw.ReadBytesFromBuffer(16))
	if err != nil {
		return nil, fmt.Errorf("parse uuid: %w", err)
	}
	uuidParsed := strfmt.UUID(uuidObj.String())

	createTime := int64(rw.ReadUint64())
	updateTime := int64(rw.ReadUint64())
	vectorLength := rw.ReadUint16()
	// The vector length should always be returned (for usage metrics purposes) even if the vector itself is skipped
	ko.VectorLen = int(vectorLength)
	if addProp.Vector {
		ko.Object.Vector = make([]float32, vectorLength)
		vectorBytes := rw.ReadBytesFromBuffer(uint64(vectorLength) * 4)
		for i := 0; i < int(vectorLength); i++ {
			bits := binary.LittleEndian.Uint32(vectorBytes[i*4 : (i+1)*4])
			ko.Object.Vector[i] = math.Float32frombits(bits)
		}
	} else {
		rw.MoveBufferPositionForward(uint64(vectorLength) * 4)
		ko.Object.Vector = nil
	}
	ko.Vector = ko.Object.Vector

	classNameLen := rw.ReadUint16()
	className := string(rw.ReadBytesFromBuffer(uint64(classNameLen)))

	propLength := rw.ReadUint32()
	var props []byte
	if addProp.NoProps {
		rw.MoveBufferPositionForward(uint64(propLength))
	} else {
		props = rw.ReadBytesFromBuffer(uint64(propLength))
	}

	var meta []byte
	metaLength := rw.ReadUint32()
	if addProp.Classification || len(addProp.ModuleParams) > 0 {
		meta = rw.ReadBytesFromBuffer(uint64(metaLength))
	} else {
		rw.MoveBufferPositionForward(uint64(metaLength))
	}

	vectorWeightsLength := rw.ReadUint32()
	vectorWeights := rw.ReadBytesFromBuffer(uint64(vectorWeightsLength))

	if len(addProp.Vectors) > 0 {
		vectors, err := unmarshalTargetVectors(&rw)
		if err != nil {
			return nil, err
		}
		ko.Vectors = vectors

		if vectors != nil {
			ko.Object.Vectors = make(models.Vectors)
			for vecName, vec := range vectors {
				ko.Object.Vectors[vecName] = vec
			}
		}
	}

	// some object members need additional "enrichment". Only do this if necessary, ie if they are actually present
	if len(props) > 0 ||
		len(meta) > 0 ||
		vectorWeightsLength > 0 &&
			!( // if the length is 4 and the encoded value is "null" (in ascii), vectorweights are not actually present
			vectorWeightsLength == 4 &&
				vectorWeights[0] == 110 && // n
				vectorWeights[1] == 117 && // u
				vectorWeights[2] == 108 && // l
				vectorWeights[3] == 108) { // l

		if err := ko.parseObject(
			uuidParsed,
			createTime,
			updateTime,
			className,
			props,
			meta,
			vectorWeights,
			properties,
			propLength,
		); err != nil {
			return nil, errors.Wrap(err, "parse")
		}
	} else {
		ko.Object.ID = uuidParsed
		ko.Object.CreationTimeUnix = createTime
		ko.Object.LastUpdateTimeUnix = updateTime
		ko.Object.Class = className
	}

	return ko, nil
}

type PropertyExtraction struct {
	PropStrings     []string
	PropStringsList [][]string
}

type bucket interface {
	GetBySecondary(int, []byte) ([]byte, error)
	GetBySecondaryWithBuffer(int, []byte, []byte) ([]byte, []byte, error)
}

func ObjectsByDocID(bucket bucket, ids []uint64,
	additional additional.Properties, properties []string, logger logrus.FieldLogger,
) ([]*Object, error) {
	if len(ids) == 1 { // no need to try to run concurrently if there is just one result anyway
		return objectsByDocIDSequential(bucket, ids, additional, properties)
	}

	return objectsByDocIDParallel(bucket, ids, additional, properties, logger)
}

func objectsByDocIDParallel(bucket bucket, ids []uint64,
	addProp additional.Properties, properties []string, logger logrus.FieldLogger,
) ([]*Object, error) {
	parallel := 2 * runtime.GOMAXPROCS(0)

	out := make([]*Object, len(ids))

	chunkSize := max(int(math.Ceil(float64(len(ids))/float64(parallel))), 1)

	eg := errwrap.NewErrorGroupWrapper(logger)

	// prevent unbounded concurrency on massive chunks
	// it's fine to use a multiple of GOMAXPROCS here, as the goroutines are
	// mostly IO-bound
	eg.SetLimit(parallel)
	for chunk := 0; chunk < parallel; chunk++ {
		start := chunk * chunkSize
		end := start + chunkSize
		if end > len(ids) {
			end = len(ids)
		}

		if start >= len(ids) {
			break
		}

		eg.Go(func() error {
			objs, err := objectsByDocIDSequential(bucket, ids[start:end], addProp, properties)
			if err != nil {
				return err
			}
			copy(out[start:end], objs)
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return out, nil
}

func objectsByDocIDSequential(bucket bucket, ids []uint64,
	additional additional.Properties, properties []string,
) ([]*Object, error) {
	if bucket == nil {
		return nil, fmt.Errorf("objects bucket not found")
	}

	var (
		docIDBuf = make([]byte, 8)
		out      = make([]*Object, len(ids))
		i        = 0
		lsmBuf   = bufPool.Get()
	)

	defer func() {
		bufPool.Put(lsmBuf)
	}()

	var props *PropertyExtraction = nil
	// not all code paths forward the list of properties that should be extracted - if nil is passed fall back
	if properties != nil {
		propStrings := make([]string, len(properties))
		propStringsList := make([][]string, len(properties))
		for j := range properties {
			propStrings[j] = properties[j]
			propStringsList[j] = []string{properties[j]}
		}

		props = &PropertyExtraction{
			PropStrings:     propStrings,
			PropStringsList: propStringsList,
		}
	}

	for _, id := range ids {
		binary.LittleEndian.PutUint64(docIDBuf, id)
		res, newBuf, err := bucket.GetBySecondaryWithBuffer(0, docIDBuf, lsmBuf)
		if err != nil {
			return nil, err
		}

		lsmBuf = newBuf // may have changed, e.g. because it was grown

		if res == nil {
			continue
		}

		unmarshalled, err := FromBinaryOptional(res, additional, props)
		if err != nil {
			return nil, errors.Wrapf(err, "unmarshal data object at position %d", i)
		}

		out[i] = unmarshalled
		i++
	}

	return out[:i], nil
}

func (ko *Object) Class() schema.ClassName {
	return schema.ClassName(ko.Object.Class)
}

func (ko *Object) SetDocID(id uint64) {
	ko.DocID = id
}

func (ko *Object) GetDocID() uint64 {
	return ko.DocID
}

func (ko *Object) CreationTimeUnix() int64 {
	return ko.Object.CreationTimeUnix
}

func (ko *Object) ExplainScore() string {
	props := ko.AdditionalProperties()
	if props != nil {
		iface := props["explainScore"]
		if iface != nil {
			return iface.(string)
		}
	}
	return ""
}

func (ko *Object) ID() strfmt.UUID {
	return ko.Object.ID
}

func (ko *Object) SetID(id strfmt.UUID) {
	ko.Object.ID = id
}

func (ko *Object) SetClass(class string) {
	ko.Object.Class = class
}

func (ko *Object) LastUpdateTimeUnix() int64 {
	return ko.Object.LastUpdateTimeUnix
}

// AdditionalProperties groups all properties which are stored with the
// object and not generated at runtime
func (ko *Object) AdditionalProperties() models.AdditionalProperties {
	return ko.Object.Additional
}

func (ko *Object) Properties() models.PropertySchema {
	return ko.Object.Properties
}

func (ko *Object) PropertiesWithAdditional(
	additional additional.Properties,
) models.PropertySchema {
	properties := ko.Properties()

	if additional.RefMeta {
		// nothing to remove
		return properties
	}

	asMap, ok := properties.(map[string]interface{})
	if !ok || asMap == nil {
		return properties
	}

	for propName, value := range asMap {
		asRefs, ok := value.(models.MultipleRef)
		if !ok {
			// not a ref, we can skip
			continue
		}

		for i := range asRefs {
			asRefs[i].Classification = nil
		}

		asMap[propName] = asRefs
	}

	return asMap
}

func (ko *Object) SetProperties(schema models.PropertySchema) {
	ko.Object.Properties = schema
}

func (ko *Object) VectorWeights() models.VectorWeights {
	return ko.Object.VectorWeights
}

func (ko *Object) SearchResult(additional additional.Properties, tenant string) *search.Result {
	propertiesMap, ok := ko.PropertiesWithAdditional(additional).(map[string]interface{})
	if !ok || propertiesMap == nil {
		propertiesMap = map[string]interface{}{}
	}
	propertiesMap["id"] = ko.ID()
	ko.SetProperties(propertiesMap)

	additionalProperties := models.AdditionalProperties{}
	if ko.AdditionalProperties() != nil {
		if interpretation, ok := additional.ModuleParams["interpretation"]; ok {
			if interpretationValue, ok := interpretation.(bool); ok && interpretationValue {
				additionalProperties["interpretation"] = ko.AdditionalProperties()["interpretation"]
			}
		}
		if additional.Classification {
			additionalProperties["classification"] = ko.AdditionalProperties()["classification"]
		}
		if additional.Group {
			additionalProperties["group"] = ko.AdditionalProperties()["group"]
		}
	}
	if ko.ExplainScore() != "" {
		additionalProperties["explainScore"] = ko.ExplainScore()
	}

	return &search.Result{
		ID:        ko.ID(),
		DocID:     &ko.DocID,
		ClassName: ko.Class().String(),
		Schema:    ko.Properties(),
		Vector:    ko.Vector,
		Vectors:   ko.asVectors(ko.Vectors),
		Dims:      ko.VectorLen,
		// VectorWeights: ko.VectorWeights(), // TODO: add vector weights
		Created:              ko.CreationTimeUnix(),
		Updated:              ko.LastUpdateTimeUnix(),
		AdditionalProperties: additionalProperties,
		// Score is filled in later
		ExplainScore: ko.ExplainScore(),
		IsConsistent: ko.IsConsistent,
		Tenant:       tenant, // not part of the binary
		// TODO: Beacon?
	}
}

func (ko *Object) asVectors(in map[string][]float32) models.Vectors {
	if len(in) > 0 {
		out := make(models.Vectors)
		for targetVector, vector := range in {
			out[targetVector] = vector
		}
		return out
	}
	return nil
}

func (ko *Object) SearchResultWithDist(addl additional.Properties, dist float32) search.Result {
	res := ko.SearchResult(addl, "")
	res.Dist = dist
	res.Certainty = float32(additional.DistToCertainty(float64(dist)))
	return *res
}

func (ko *Object) SearchResultWithScore(addl additional.Properties, score float32) search.Result {
	res := ko.SearchResult(addl, "")
	res.Score = score
	return *res
}

func (ko *Object) SearchResultWithScoreAndTenant(addl additional.Properties, score float32, tenant string) search.Result {
	res := ko.SearchResult(addl, tenant)
	res.Score = score
	return *res
}

func (ko *Object) Valid() bool {
	return ko.ID() != "" &&
		ko.Class().String() != ""
}

func SearchResults(in []*Object, additional additional.Properties, tenant string) search.Results {
	out := make(search.Results, len(in))

	for i, elem := range in {
		out[i] = *(elem.SearchResult(additional, tenant))
	}

	return out
}

func SearchResultsWithScore(in []*Object, scores []float32, additional additional.Properties, tenant string) search.Results {
	out := make(search.Results, len(in))

	for i, elem := range in {
		score := float32(0.0)
		if len(scores) > i {
			score = scores[i]
		}
		out[i] = elem.SearchResultWithScoreAndTenant(additional, score, tenant)
	}

	return out
}

func SearchResultsWithDists(in []*Object, addl additional.Properties,
	dists []float32,
) search.Results {
	out := make(search.Results, len(in))

	for i, elem := range in {
		out[i] = elem.SearchResultWithDist(addl, dists[i])
	}

	return out
}

func DocIDFromBinary(in []byte) (uint64, error) {
	if len(in) < 9 {
		return 0, errors.Errorf("binary data too short")
	}
	// first by is kind, then 8 bytes for the docID
	return binary.LittleEndian.Uint64(in[1:9]), nil
}

func DocIDAndTimeFromBinary(in []byte) (docID uint64, updateTime int64, err error) {
	r := bytes.NewReader(in)

	var version uint8

	le := binary.LittleEndian

	if err := binary.Read(r, le, &version); err != nil {
		return 0, 0, err
	}

	if version != 1 {
		return 0, 0, errors.Errorf("unsupported binary marshaller version %d", version)
	}

	err = binary.Read(r, le, &docID)
	if err != nil {
		return 0, 0, err
	}

	var buf [1 + 16 + 8 + 8]byte // kind uuid createtime updatetime

	_, err = io.ReadFull(r, buf[:])
	if err != nil {
		return 0, 0, err
	}

	updateTime = int64(binary.LittleEndian.Uint64(buf[1+16+8:]))

	return docID, updateTime, nil
}

// MarshalBinary creates the binary representation of a kind object. Regardless
// of the marshaller version the first byte is a uint8 indicating the version
// followed by the payload which depends on the specific version
//
// Version 1
// No. of B   | Type          | Content
// ------------------------------------------------
// 1          | uint8         | MarshallerVersion = 1
// 8          | uint64        | index id, keep early so id-only lookups are maximum efficient
// 1          | uint8         | kind, 0=action, 1=thing - deprecated
// 16         | uint128       | uuid
// 8          | int64         | create time
// 8          | int64         | update time
// 2          | uint16        | VectorLength
// n*4        | []float32     | vector of length n
// 2          | uint16        | length of class name
// n          | []byte        | className
// 4          | uint32        | length of schema json
// n          | []byte        | schema as json
// 4          | uint32        | length of meta json
// n          | []byte        | meta as json
// 4          | uint32        | length of vectorweights json
// n          | []byte        | vectorweights as json
// 4          | uint32        | length of packed target vectors offsets (in bytes)
// n          | []byte        | packed target vectors offsets map { name : offset_in_bytes }
// 4          | uint32        | length of target vectors segment (in bytes)
// n          | uint16+[]byte | target vectors segment: sequence of vec_length + vec (uint16 + []byte), (uint16 + []byte) ...

const (
	maxVectorLength               int = math.MaxUint16
	maxClassNameLength            int = math.MaxUint16
	maxSchemaLength               int = math.MaxUint32
	maxMetaLength                 int = math.MaxUint32
	maxVectorWeightsLength        int = math.MaxUint32
	maxTargetVectorsSegmentLength int = math.MaxUint32
	maxTargetVectorsOffsetsLength int = math.MaxUint32
)

func (ko *Object) MarshalBinary() ([]byte, error) {
	if ko.MarshallerVersion != 1 {
		return nil, errors.Errorf("unsupported marshaller version %d", ko.MarshallerVersion)
	}

	kindByte := uint8(0)
	// Deprecated Kind field
	kindByte = 1

	idParsed, err := uuid.Parse(ko.ID().String())
	if err != nil {
		return nil, err
	}
	idBytes, err := idParsed.MarshalBinary()
	if err != nil {
		return nil, err
	}

	if len(ko.Vector) > maxVectorLength {
		return nil, fmt.Errorf("could not marshal '%s' max length exceeded (%d/%d)", "vector", len(ko.Vector), maxVectorLength)
	}
	vectorLength := uint32(len(ko.Vector))

	className := []byte(ko.Class())
	if len(className) > maxClassNameLength {
		return nil, fmt.Errorf("could not marshal '%s' max length exceeded (%d/%d)", "className", len(className), maxClassNameLength)
	}
	classNameLength := uint32(len(className))

	schema, err := json.Marshal(ko.Properties())
	if err != nil {
		return nil, err
	}
	if len(schema) > maxSchemaLength {
		return nil, fmt.Errorf("could not marshal '%s' max length exceeded (%d/%d)", "schema", len(schema), maxSchemaLength)
	}
	schemaLength := uint32(len(schema))

	meta, err := json.Marshal(ko.AdditionalProperties())
	if err != nil {
		return nil, err
	}
	if len(meta) > maxMetaLength {
		return nil, fmt.Errorf("could not marshal '%s' max length exceeded (%d/%d)", "meta", len(meta), maxMetaLength)
	}
	metaLength := uint32(len(meta))

	vectorWeights, err := json.Marshal(ko.VectorWeights())
	if err != nil {
		return nil, err
	}
	if len(vectorWeights) > maxVectorWeightsLength {
		return nil, fmt.Errorf("could not marshal '%s' max length exceeded (%d/%d)", "vectorWeights", len(vectorWeights), maxVectorWeightsLength)
	}
	vectorWeightsLength := uint32(len(vectorWeights))

	var targetVectorsOffsets []byte
	var targetVectorsOffsetsLength uint32
	var targetVectorsSegmentLength int

	targetVectorsOffsetOrder := make([]string, 0, len(ko.Vectors))
	if len(ko.Vectors) > 0 {
		offsetsMap := map[string]uint32{}
		for name, vec := range ko.Vectors {
			if len(vec) > maxVectorLength {
				return nil, fmt.Errorf("could not marshal '%s' max length exceeded (%d/%d)", "vector", len(vec), maxVectorLength)
			}

			offsetsMap[name] = uint32(targetVectorsSegmentLength)
			targetVectorsSegmentLength += 2 + 4*len(vec) // 2 for vec length + vec bytes

			if targetVectorsSegmentLength > maxTargetVectorsSegmentLength {
				return nil,
					fmt.Errorf("could not marshal '%s' max length exceeded (%d/%d)",
						"targetVectorsSegmentLength", targetVectorsSegmentLength, maxTargetVectorsSegmentLength)
			}

			targetVectorsOffsetOrder = append(targetVectorsOffsetOrder, name)
		}

		targetVectorsOffsets, err = msgpack.Marshal(offsetsMap)
		if err != nil {
			return nil, fmt.Errorf("could not marshal target vectors offsets: %w", err)
		}
		if len(targetVectorsOffsets) > maxTargetVectorsOffsetsLength {
			return nil, fmt.Errorf("could not marshal '%s' max length exceeded (%d/%d)", "targetVectorsOffsets", len(targetVectorsOffsets), maxTargetVectorsOffsetsLength)
		}
		targetVectorsOffsetsLength = uint32(len(targetVectorsOffsets))
	}

	totalBufferLength := 1 + 8 + 1 + 16 + 8 + 8 +
		2 + vectorLength*4 +
		2 + classNameLength +
		4 + schemaLength +
		4 + metaLength +
		4 + vectorWeightsLength +
		4 + targetVectorsOffsetsLength +
		4 + uint32(targetVectorsSegmentLength)

	byteBuffer := make([]byte, totalBufferLength)
	rw := byteops.NewReadWriter(byteBuffer)
	rw.WriteByte(ko.MarshallerVersion)
	rw.WriteUint64(ko.DocID)
	rw.WriteByte(kindByte)

	rw.CopyBytesToBuffer(idBytes)

	rw.WriteUint64(uint64(ko.CreationTimeUnix()))
	rw.WriteUint64(uint64(ko.LastUpdateTimeUnix()))
	rw.WriteUint16(uint16(vectorLength))

	for j := uint32(0); j < vectorLength; j++ {
		rw.WriteUint32(math.Float32bits(ko.Vector[j]))
	}

	rw.WriteUint16(uint16(classNameLength))
	err = rw.CopyBytesToBuffer(className)
	if err != nil {
		return byteBuffer, errors.Wrap(err, "Could not copy className")
	}

	rw.WriteUint32(schemaLength)
	err = rw.CopyBytesToBuffer(schema)
	if err != nil {
		return byteBuffer, errors.Wrap(err, "Could not copy schema")
	}

	rw.WriteUint32(metaLength)
	err = rw.CopyBytesToBuffer(meta)
	if err != nil {
		return byteBuffer, errors.Wrap(err, "Could not copy meta")
	}

	rw.WriteUint32(vectorWeightsLength)
	err = rw.CopyBytesToBuffer(vectorWeights)
	if err != nil {
		return byteBuffer, errors.Wrap(err, "Could not copy vectorWeights")
	}

	rw.WriteUint32(targetVectorsOffsetsLength)
	if targetVectorsOffsetsLength > 0 {
		err = rw.CopyBytesToBuffer(targetVectorsOffsets)
		if err != nil {
			return byteBuffer, errors.Wrap(err, "Could not copy targetVectorsOffsets")
		}
	}

	rw.WriteUint32(uint32(targetVectorsSegmentLength))
	for _, name := range targetVectorsOffsetOrder {
		vec := ko.Vectors[name]
		vecLen := len(vec)

		rw.WriteUint16(uint16(vecLen))
		for j := 0; j < vecLen; j++ {
			rw.WriteUint32(math.Float32bits(vec[j]))
		}
	}

	return byteBuffer, nil
}

// UnmarshalPropertiesFromObject only unmarshals and returns the properties part of the object
//
// Check MarshalBinary for the order of elements in the input array
func UnmarshalPropertiesFromObject(data []byte, properties *map[string]interface{}, aggregationProperties []string, propStrings [][]string) error {
	if data[0] != uint8(1) {
		return errors.Errorf("unsupported binary marshaller version %d", data[0])
	}

	// clear out old values in case an object misses values. This should NOT shrink the capacity of the map, eg there
	// are no allocations when adding the properties of the next object again
	for k := range *properties {
		delete(*properties, k)
	}

	startPos := uint64(1 + 8 + 1 + 16 + 8 + 8) // elements at the start
	rw := byteops.NewReadWriter(data, byteops.WithPosition(startPos))
	// get the length of the vector, each element is a float32 (4 bytes)
	vectorLength := uint64(rw.ReadUint16())
	rw.MoveBufferPositionForward(vectorLength * 4)

	classnameLength := uint64(rw.ReadUint16())
	rw.MoveBufferPositionForward(classnameLength)
	propertyLength := uint64(rw.ReadUint32())

	return UnmarshalProperties(rw.Buffer[rw.Position:rw.Position+propertyLength], properties, aggregationProperties, propStrings)
}

func UnmarshalProperties(data []byte, properties *map[string]interface{}, aggregationProperties []string, propStrings [][]string) error {
	var returnError error
	jsonparser.EachKey(data, func(idx int, value []byte, dataType jsonparser.ValueType, err error) {
		switch dataType {
		case jsonparser.Number, jsonparser.String, jsonparser.Boolean:
			val, err := parseValues(dataType, value)
			if err != nil {
				returnError = err
			}
			(*properties)[aggregationProperties[idx]] = val
		case jsonparser.Array: // can be a beacon or an actual array
			arrayEntries := value[1 : len(value)-1] // without leading and trailing []
			// this checks if refs are present - the return points to the underlying memory, dont use without copying
			_, errBeacon := jsonparser.GetUnsafeString(arrayEntries, "beacon")
			if errBeacon == nil {
				// there can be more than one
				var beacons []interface{}
				handler := func(beaconByte []byte, dataType jsonparser.ValueType, offset int, err error) {
					beaconVal, err2 := jsonparser.GetString(beaconByte, "beacon") // this points to the underlying memory
					returnError = err2
					beacons = append(beacons, map[string]interface{}{"beacon": beaconVal})
				}
				_, returnError = jsonparser.ArrayEach(value, handler)
				(*properties)[aggregationProperties[idx]] = beacons
			} else {
				// check how many entries there are in the array by counting the ",". This allows us to allocate an
				// array with the right size without extending it with every append.
				// The size can be too large for string arrays, when they contain "," as part of their content.
				entryCount := 0
				for _, b := range arrayEntries {
					if b == uint8(44) { // ',' as byte
						entryCount++
					}
				}

				array := make([]interface{}, 0, entryCount)
				_, err = jsonparser.ArrayEach(value, func(innerValue []byte, innerDataType jsonparser.ValueType, offset int, innerErr error) {
					var val interface{}

					switch innerDataType {
					case jsonparser.Number, jsonparser.String, jsonparser.Boolean:
						val, err = parseValues(innerDataType, innerValue)
						if err != nil {
							returnError = err
						}
					default:
						returnError = fmt.Errorf("unknown data type ArrayEach %v", innerDataType)
					}
					array = append(array, val)
				})
				if err != nil {
					returnError = err
				}
				(*properties)[aggregationProperties[idx]] = array

			}
		case jsonparser.Object:
			// nested objects and geo-props and phonenumbers.
			//
			// we do not have the schema for nested object and cannot use the efficient jsonparser for them
			//  (we could for phonenumbers and geo-props but they are not worth the effort)
			// however this part is only called if
			// - one of the datatypes is present
			// - AND the user requests them
			// => the performance impact is minimal
			nestedProps := map[string]interface{}{}
			err := json.Unmarshal(value, &nestedProps)
			if err != nil {
				returnError = err
			}
			(*properties)[aggregationProperties[idx]] = nestedProps
		default:
			returnError = fmt.Errorf("unknown data type %v", dataType)
		}
	}, propStrings...)

	return returnError
}

func parseValues(dt jsonparser.ValueType, value []byte) (interface{}, error) {
	switch dt {
	case jsonparser.Number:
		return jsonparser.ParseFloat(value)
	case jsonparser.String:
		return jsonparser.ParseString(value)
	case jsonparser.Boolean:
		return jsonparser.ParseBoolean(value)
	default:
		panic("Unknown data type") // returning an error would be better
	}
}

// UnmarshalBinary is the versioned way to unmarshal a kind object from binary,
// see MarshalBinary for the exact contents of each version
func (ko *Object) UnmarshalBinary(data []byte) error {
	version := data[0]
	if version != 1 {
		return errors.Errorf("unsupported binary marshaller version %d", version)
	}
	ko.MarshallerVersion = version

	rw := byteops.NewReadWriter(data, byteops.WithPosition(1))
	ko.DocID = rw.ReadUint64()
	rw.MoveBufferPositionForward(1) // kind-byte

	uuidParsed, err := uuid.FromBytes(data[rw.Position : rw.Position+16])
	if err != nil {
		return err
	}
	rw.MoveBufferPositionForward(16)

	createTime := int64(rw.ReadUint64())
	updateTime := int64(rw.ReadUint64())

	vectorLength := rw.ReadUint16()
	ko.VectorLen = int(vectorLength)
	ko.Vector = make([]float32, vectorLength)
	for j := 0; j < int(vectorLength); j++ {
		ko.Vector[j] = math.Float32frombits(rw.ReadUint32())
	}

	classNameLength := uint64(rw.ReadUint16())
	className, err := rw.CopyBytesFromBuffer(classNameLength, nil)
	if err != nil {
		return errors.Wrap(err, "Could not copy class name")
	}

	schemaLength := uint64(rw.ReadUint32())
	schema, err := rw.CopyBytesFromBuffer(schemaLength, nil)
	if err != nil {
		return errors.Wrap(err, "Could not copy schema")
	}

	metaLength := uint64(rw.ReadUint32())
	meta, err := rw.CopyBytesFromBuffer(metaLength, nil)
	if err != nil {
		return errors.Wrap(err, "Could not copy meta")
	}

	vectorWeightsLength := uint64(rw.ReadUint32())
	vectorWeights, err := rw.CopyBytesFromBuffer(vectorWeightsLength, nil)
	if err != nil {
		return errors.Wrap(err, "Could not copy vectorWeights")
	}

	vectors, err := unmarshalTargetVectors(&rw)
	if err != nil {
		return err
	}
	ko.Vectors = vectors

	return ko.parseObject(
		strfmt.UUID(uuidParsed.String()),
		createTime,
		updateTime,
		string(className),
		schema,
		meta,
		vectorWeights, nil, 0,
	)
}

func unmarshalTargetVectors(rw *byteops.ReadWriter) (map[string][]float32, error) {
	// This check prevents from panic when somebody is upgrading from version that
	// didn't have multi vector support. This check is needed bc with named vectors
	// feature storage object can have vectors data appended at the end of the file
	if rw.Position < uint64(len(rw.Buffer)) {
		targetVectorsOffsets := rw.ReadBytesFromBufferWithUint32LengthIndicator()
		targetVectorsSegmentLength := rw.ReadUint32()
		pos := rw.Position

		if len(targetVectorsOffsets) > 0 {
			var tvOffsets map[string]uint32
			if err := msgpack.Unmarshal(targetVectorsOffsets, &tvOffsets); err != nil {
				return nil, fmt.Errorf("Could not unmarshal target vectors offset: %w", err)
			}

			targetVectors := map[string][]float32{}
			for name, offset := range tvOffsets {
				rw.MoveBufferToAbsolutePosition(pos + uint64(offset))
				vecLen := rw.ReadUint16()
				vec := make([]float32, vecLen)
				for j := uint16(0); j < vecLen; j++ {
					vec[j] = math.Float32frombits(rw.ReadUint32())
				}
				targetVectors[name] = vec
			}

			rw.MoveBufferToAbsolutePosition(pos + uint64(targetVectorsSegmentLength))
			return targetVectors, nil
		}
	}
	return nil, nil
}

func VectorFromBinary(in []byte, buffer []float32, targetVector string) ([]float32, error) {
	if len(in) == 0 {
		return nil, nil
	}

	version := in[0]
	if version != 1 {
		return nil, errors.Errorf("unsupported marshaller version %d", version)
	}

	if targetVector != "" {
		startPos := uint64(1 + 8 + 1 + 16 + 8 + 8) // elements at the start
		rw := byteops.NewReadWriter(in, byteops.WithPosition(startPos))

		vectorLength := uint64(rw.ReadUint16())
		rw.MoveBufferPositionForward(vectorLength * 4)

		classnameLength := uint64(rw.ReadUint16())
		rw.MoveBufferPositionForward(classnameLength)

		schemaLength := uint64(rw.ReadUint32())
		rw.MoveBufferPositionForward(schemaLength)

		metaLength := uint64(rw.ReadUint32())
		rw.MoveBufferPositionForward(metaLength)

		vectorWeightsLength := uint64(rw.ReadUint32())
		rw.MoveBufferPositionForward(vectorWeightsLength)

		targetVectors, err := unmarshalTargetVectors(&rw)
		if err != nil {
			return nil, errors.Errorf("unable to unmarshal vector for target vector: %s", targetVector)
		}
		vector, ok := targetVectors[targetVector]
		if !ok {
			return nil, errors.Errorf("vector not found for target vector: %s", targetVector)
		}
		return vector, nil
	}

	// since we know the version and know that the blob is not len(0), we can
	// assume that we can directly access the vector length field. The only
	// situation where this is not accessible would be on corrupted data - where
	// it would be acceptable to panic
	vecLen := binary.LittleEndian.Uint16(in[42:44])

	var out []float32
	if cap(buffer) >= int(vecLen) {
		out = buffer[:vecLen]
	} else {
		out = make([]float32, vecLen)
	}
	vecStart := 44
	vecEnd := vecStart + int(vecLen*4)

	i := 0
	for start := vecStart; start < vecEnd; start += 4 {
		asUint := binary.LittleEndian.Uint32(in[start : start+4])
		out[i] = math.Float32frombits(asUint)
		i++
	}

	return out, nil
}

func (ko *Object) parseObject(uuid strfmt.UUID, create, update int64, className string,
	propsB []byte, additionalB []byte, vectorWeightsB []byte, properties *PropertyExtraction, propLength uint32,
) error {
	var returnProps map[string]interface{}
	if properties == nil || propLength == 0 {
		if err := json.Unmarshal(propsB, &returnProps); err != nil {
			return err
		}
	} else if len(propsB) >= int(propLength) {
		// the properties are not read in all cases, skip if not needed
		returnProps = make(map[string]interface{}, len(properties.PropStrings))
		if err := UnmarshalProperties(propsB[:propLength], &returnProps, properties.PropStrings, properties.PropStringsList); err != nil {
			return err
		}
	}

	if err := enrichSchemaTypes(returnProps, false); err != nil {
		return errors.Wrap(err, "enrich schema datatypes")
	}

	var additionalProperties models.AdditionalProperties
	if len(additionalB) > 0 {
		if err := json.Unmarshal(additionalB, &additionalProperties); err != nil {
			return err
		}

		if prop, ok := additionalProperties["classification"]; ok {
			if classificationMap, ok := prop.(map[string]interface{}); ok {
				marshalled, err := json.Marshal(classificationMap)
				if err != nil {
					return err
				}
				var classification additional.Classification
				err = json.Unmarshal(marshalled, &classification)
				if err != nil {
					return err
				}
				additionalProperties["classification"] = &classification
			}
		}

		if prop, ok := additionalProperties["group"]; ok {
			if groupMap, ok := prop.(map[string]interface{}); ok {
				marshalled, err := json.Marshal(groupMap)
				if err != nil {
					return err
				}
				var group additional.Group
				err = json.Unmarshal(marshalled, &group)
				if err != nil {
					return err
				}

				for i, hit := range group.Hits {
					if groupHitAdditionalMap, ok := hit["_additional"].(map[string]interface{}); ok {
						marshalled, err := json.Marshal(groupHitAdditionalMap)
						if err != nil {
							return err
						}
						var groupHitsAdditional additional.GroupHitAdditional
						err = json.Unmarshal(marshalled, &groupHitsAdditional)
						if err != nil {
							return err
						}
						group.Hits[i]["_additional"] = &groupHitsAdditional
					}
				}

				additionalProperties["group"] = &group
			}
		}
	}

	var vectorWeights interface{}
	if err := json.Unmarshal(vectorWeightsB, &vectorWeights); err != nil {
		return err
	}

	ko.Object = models.Object{
		Class:              className,
		CreationTimeUnix:   create,
		LastUpdateTimeUnix: update,
		ID:                 uuid,
		Properties:         returnProps,
		VectorWeights:      vectorWeights,
		Additional:         additionalProperties,
	}

	return nil
}

// DeepCopyDangerous creates a deep copy of the underlying Object
// WARNING: This was purpose built for the batch ref usecase and only covers
// the situations that are required there. This means that cases which aren't
// reflected in that usecase may still contain references. Thus the suffix
// "Dangerous". If needed, make sure everything is copied and remove the
// suffix.
func (ko *Object) DeepCopyDangerous() *Object {
	o := &Object{
		MarshallerVersion: ko.MarshallerVersion,
		DocID:             ko.DocID,
		Object:            deepCopyObject(ko.Object),
		Vector:            deepCopyVector(ko.Vector),
		Vectors:           deepCopyVectors(ko.Vectors),
	}

	return o
}

func AddOwnership(objs []*Object, node, shard string) {
	for i := range objs {
		objs[i].BelongsToNode = node
		objs[i].BelongsToShard = shard
	}
}

func deepCopyVector(orig []float32) []float32 {
	out := make([]float32, len(orig))
	copy(out, orig)
	return out
}

func deepCopyVectors[V []float32 | models.Vector](orig map[string]V) map[string]V {
	out := make(map[string]V, len(orig))
	for key, vec := range orig {
		out[key] = deepCopyVector(vec)
	}
	return out
}

func deepCopyObject(orig models.Object) models.Object {
	return models.Object{
		Class:              orig.Class,
		ID:                 orig.ID,
		CreationTimeUnix:   orig.CreationTimeUnix,
		LastUpdateTimeUnix: orig.LastUpdateTimeUnix,
		Vector:             deepCopyVector(orig.Vector),
		VectorWeights:      orig.VectorWeights,
		Additional:         orig.Additional, // WARNING: not a deep copy!!
		Properties:         deepCopyProperties(orig.Properties),
		Vectors:            deepCopyVectors(orig.Vectors),
	}
}

func deepCopyProperties(orig models.PropertySchema) models.PropertySchema {
	if orig == nil {
		return nil
	}

	asMap, ok := orig.(map[string]interface{})
	if !ok {
		// not a map, don't know what to do with this
		return nil
	}

	out := map[string]interface{}{}

	for key, value := range asMap {
		if mref, ok := value.(models.MultipleRef); ok {
			out[key] = deepCopyMRef(mref)
			continue
		}

		// Note: This is not a true deep copy, value could still be a pointer type,
		// such as *models.GeoCoordinates, thus leading to passing a reference
		// instead of actually making a copy. However, for the purposes we need
		// this method for this is acceptable based on our current knowledge
		out[key] = value
	}

	return out
}

func deepCopyMRef(orig models.MultipleRef) models.MultipleRef {
	if orig == nil {
		return nil
	}

	out := make(models.MultipleRef, len(orig))
	for i, ref := range orig {
		// models.SingleRef contains only pass-by-value props, so a simple deref as
		// the struct creates a copy
		copiedRef := *ref
		out[i] = &copiedRef
	}

	return out
}
