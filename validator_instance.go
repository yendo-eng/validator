package validator

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

const (
	defaultTagName     = "validate"
	utf8HexComma       = "0x2C"
	utf8Pipe           = "0x7C"
	tagSeparator       = ","
	orSeparator        = "|"
	tagKeySeparator    = "="
	structOnlyTag      = "structonly"
	noStructLevelTag   = "nostructlevel"
	omitempty          = "omitempty"
	skipValidationTag  = "-"
	diveTag            = "dive"
	namespaceSeparator = "."
	leftBracket        = "["
	rightBracket       = "]"
	restrictedTagChars = ".[],|=+()`~!@#$%^&*\\\"/?<>{}"
	restrictedAliasErr = "Alias '%s' either contains restricted characters or is the same as a restricted tag needed for normal operation"
	restrictedTagErr   = "Tag '%s' either contains restricted characters or is the same as a restricted tag needed for normal operation"
)

var (
	timeType      = reflect.TypeOf(time.Time{})
	defaultCField = new(cField)
)

// CustomTypeFunc allows for overriding or adding custom field type handler functions
// field = field value of the type to return a value to be validated
// example Valuer from sql drive see https://golang.org/src/database/sql/driver/types.go?s=1210:1293#L29
type CustomTypeFunc func(field reflect.Value) interface{}

// TagNameFunc allows for adding of a custom tag name parser
type TagNameFunc func(field reflect.StructField) string

// Validate contains the validator settings and cache
type Validate struct {
	tagName          string
	pool             *sync.Pool
	hasCustomFuncs   bool
	hasTagNameFunc   bool
	tagNameFunc      TagNameFunc
	structLevelFuncs map[reflect.Type]StructLevelFunc
	customFuncs      map[reflect.Type]CustomTypeFunc
	aliases          map[string]string
	validations      map[string]Func
	tagCache         *tagCache
	structCache      *structCache
}

// New returns a new instacne of 'validate' with sane defaults.
func New() *Validate {

	tc := new(tagCache)
	tc.m.Store(make(map[string]*cTag))

	sc := new(structCache)
	sc.m.Store(make(map[reflect.Type]*cStruct))

	v := &Validate{
		tagName:     defaultTagName,
		aliases:     make(map[string]string, len(bakedInAliases)),
		validations: make(map[string]Func, len(bakedInValidators)),
		tagCache:    tc,
		structCache: sc,
	}

	// must copy alias validators for separate validations to be used in each validator instance
	for k, val := range bakedInAliases {
		v.RegisterAlias(k, val)
	}

	// must copy validators for separate validations to be used in each instance
	for k, val := range bakedInValidators {

		// no need to error check here, baked in will alwaays be valid
		v.RegisterValidation(k, val)
	}

	v.pool = &sync.Pool{
		New: func() interface{} {
			return &validate{
				v:    v,
				errs: make(ValidationErrors, 0, 4),
			}
		},
	}

	return v
}

// SetTagName allows for changing of the default tag name of 'validate'
func (v *Validate) SetTagName(name string) {
	v.tagName = name
}

// RegisterTagNameFunc registers a function to get another name from the
// StructField eg. the JSON name
func (v *Validate) RegisterTagNameFunc(fn TagNameFunc) {
	v.tagNameFunc = fn
	v.hasTagNameFunc = true
}

// RegisterValidation adds a validation with the given tag
//
// NOTES:
// - if the key already exists, the previous validation function will be replaced.
// - this method is not thread-safe it is intended that these all be registered prior to any validation
func (v *Validate) RegisterValidation(tag string, fn Func) error {

	if len(tag) == 0 {
		return errors.New("Function Key cannot be empty")
	}

	if fn == nil {
		return errors.New("Function cannot be empty")
	}

	_, ok := restrictedTags[tag]

	if ok || strings.ContainsAny(tag, restrictedTagChars) {
		panic(fmt.Sprintf(restrictedTagErr, tag))
	}

	v.validations[tag] = fn

	return nil
}

// RegisterAlias registers a mapping of a single validation tag that
// defines a common or complex set of validation(s) to simplify adding validation
// to structs.
//
// NOTE: this function is not thread-safe it is intended that these all be registered prior to any validation
func (v *Validate) RegisterAlias(alias, tags string) {

	_, ok := restrictedTags[alias]

	if ok || strings.ContainsAny(alias, restrictedTagChars) {
		panic(fmt.Sprintf(restrictedAliasErr, alias))
	}

	v.aliases[alias] = tags
}

// RegisterStructValidation registers a StructLevelFunc against a number of types.
// This is akin to implementing the 'Validatable' interface, but for structs for which
// you may not have access or rights to change.
//
// NOTES:
// - if this and the 'Validatable' interface are implemented the Struct Level takes precedence as to enable
// a struct out of your control's validation to be overridden
// - this method is not thread-safe it is intended that these all be registered prior to any validation
func (v *Validate) RegisterStructValidation(fn StructLevelFunc, types ...interface{}) {

	if v.structLevelFuncs == nil {
		v.structLevelFuncs = make(map[reflect.Type]StructLevelFunc)
	}

	for _, t := range types {
		v.structLevelFuncs[reflect.TypeOf(t)] = fn
	}
}

// RegisterCustomTypeFunc registers a CustomTypeFunc against a number of types
//
// NOTE: this method is not thread-safe it is intended that these all be registered prior to any validation
func (v *Validate) RegisterCustomTypeFunc(fn CustomTypeFunc, types ...interface{}) {

	if v.customFuncs == nil {
		v.customFuncs = make(map[reflect.Type]CustomTypeFunc)
	}

	for _, t := range types {
		v.customFuncs[reflect.TypeOf(t)] = fn
	}

	v.hasCustomFuncs = true
}

// Struct validates a structs exposed fields, and automatically validates nested structs, unless otherwise specified.
//
// It returns InvalidValidationError for bad values passed in and nil or ValidationErrors as error otherwise.
// You will need to assert the error if it's not nil eg. err.(validator.ValidationErrors) to access the array of errors.
func (v *Validate) Struct(s interface{}) (err error) {

	val := reflect.ValueOf(s)
	top := val

	if val.Kind() == reflect.Ptr && !val.IsNil() {
		val = val.Elem()
	}

	typ := val.Type()

	if val.Kind() != reflect.Struct || typ == timeType {
		return &InvalidValidationError{Type: typ}
	}

	// good to validate
	vd := v.pool.Get().(*validate)
	vd.top = top
	vd.isPartial = false
	// vd.hasExcludes = false // only need to reset in StructPartial and StructExcept

	vd.validateStruct(top, val, typ, vd.ns[0:0], vd.actualNs[0:0], nil)

	if len(vd.errs) > 0 {
		err = vd.errs
		vd.errs = nil
	}

	v.pool.Put(vd)

	return
}

// StructPartial validates the fields passed in only, ignoring all others.
// Fields may be provided in a namespaced fashion relative to the  struct provided
// eg. NestedStruct.Field or NestedArrayField[0].Struct.Name
//
// It returns InvalidValidationError for bad values passed in and nil or ValidationErrors as error otherwise.
// You will need to assert the error if it's not nil eg. err.(validator.ValidationErrors) to access the array of errors.
func (v *Validate) StructPartial(s interface{}, fields ...string) (err error) {

	val := reflect.ValueOf(s)
	top := val

	if val.Kind() == reflect.Ptr && !val.IsNil() {
		val = val.Elem()
	}

	typ := val.Type()

	if val.Kind() != reflect.Struct || typ == timeType {
		return &InvalidValidationError{Type: typ}
	}

	// good to validate
	vd := v.pool.Get().(*validate)
	vd.top = top
	vd.isPartial = true
	vd.hasExcludes = false
	vd.includeExclude = make(map[string]struct{})

	name := typ.Name()

	if fields != nil {
		for _, k := range fields {

			flds := strings.Split(k, namespaceSeparator)
			if len(flds) > 0 {

				key := name + namespaceSeparator
				for _, s := range flds {

					idx := strings.Index(s, leftBracket)

					if idx != -1 {
						for idx != -1 {
							key += s[:idx]
							vd.includeExclude[key] = struct{}{}

							idx2 := strings.Index(s, rightBracket)
							idx2++
							key += s[idx:idx2]
							vd.includeExclude[key] = struct{}{}
							s = s[idx2:]
							idx = strings.Index(s, leftBracket)
						}
					} else {

						key += s
						vd.includeExclude[key] = struct{}{}
					}

					key += namespaceSeparator
				}
			}
		}
	}

	vd.validateStruct(top, val, typ, vd.ns[0:0], vd.actualNs[0:0], nil)

	if len(vd.errs) > 0 {
		err = vd.errs
		vd.errs = nil
	}

	v.pool.Put(vd)

	return
}

// StructExcept validates all fields except the ones passed in.
// Fields may be provided in a namespaced fashion relative to the  struct provided
// i.e. NestedStruct.Field or NestedArrayField[0].Struct.Name
//
// It returns InvalidValidationError for bad values passed in and nil or ValidationErrors as error otherwise.
// You will need to assert the error if it's not nil eg. err.(validator.ValidationErrors) to access the array of errors.
func (v *Validate) StructExcept(s interface{}, fields ...string) (err error) {

	val := reflect.ValueOf(s)
	top := val

	if val.Kind() == reflect.Ptr && !val.IsNil() {
		val = val.Elem()
	}

	typ := val.Type()

	if val.Kind() != reflect.Struct || typ == timeType {
		return &InvalidValidationError{Type: typ}
	}

	// good to validate
	vd := v.pool.Get().(*validate)
	vd.top = top
	vd.isPartial = true
	vd.hasExcludes = true
	vd.includeExclude = make(map[string]struct{})

	name := typ.Name()

	for _, key := range fields {
		vd.includeExclude[name+namespaceSeparator+key] = struct{}{}
	}

	vd.validateStruct(top, val, typ, vd.ns[0:0], vd.actualNs[0:0], nil)

	if len(vd.errs) > 0 {
		err = vd.errs
		vd.errs = nil
	}

	v.pool.Put(vd)

	return
}

// func (v *validate) traverseField(parent reflect.Value, current reflect.Value, ns []byte, actualNs []byte, cf *cField, ct *cTag) {

// Var validates a single variable using tag style validation.
// eg.
// var i int
// validate.Var(i, "gt=1,lt=10")
//
// WARNING: a struct can be passed for validation eg. time.Time is a struct or if you have a custom type and have registered
//          a custom type handler, so must allow it; however unforseen validations will occur if trying to validate a struct
//          that is meant to be passed to 'validate.Struct'
//
// It returns InvalidValidationError for bad values passed in and nil or ValidationErrors as error otherwise.
// You will need to assert the error if it's not nil eg. err.(validator.ValidationErrors) to access the array of errors.
// validate Array, Slice and maps fields which may contain more than one error
func (v *Validate) Var(field interface{}, tag string) (err error) {

	if len(tag) == 0 || tag == skipValidationTag {
		return nil
	}

	// find cached tag
	ctag, ok := v.tagCache.Get(tag)
	if !ok {
		v.tagCache.lock.Lock()
		defer v.tagCache.lock.Unlock()

		// could have been multiple trying to access, but once first is done this ensures tag
		// isn't parsed again.
		ctag, ok = v.tagCache.Get(tag)
		if !ok {
			ctag, _ = v.parseFieldTagsRecursive(tag, "", "", false)
			v.tagCache.Set(tag, ctag)
		}
	}

	val := reflect.ValueOf(field)

	vd := v.pool.Get().(*validate)
	vd.top = val
	vd.isPartial = false

	vd.traverseField(val, val, vd.ns[0:0], vd.actualNs[0:0], defaultCField, ctag)

	if len(vd.errs) > 0 {
		err = vd.errs
		vd.errs = nil
	}

	v.pool.Put(vd)

	return
}

// VarWithValue validates a single variable, against another variable/field's value using tag style validation
// eg.
// s1 := "abcd"
// s2 := "abcd"
// validate.VarWithValue(s1, s2, "eqcsfield") // returns true
//
// WARNING: a struct can be passed for validation eg. time.Time is a struct or if you have a custom type and have registered
//          a custom type handler, so must allow it; however unforseen validations will occur if trying to validate a struct
//          that is meant to be passed to 'validate.Struct'
//
// It returns InvalidValidationError for bad values passed in and nil or ValidationErrors as error otherwise.
// You will need to assert the error if it's not nil eg. err.(validator.ValidationErrors) to access the array of errors.
// validate Array, Slice and maps fields which may contain more than one error
func (v *Validate) VarWithValue(field interface{}, other interface{}, tag string) (err error) {

	if len(tag) == 0 || tag == skipValidationTag {
		return nil
	}

	// find cached tag
	ctag, ok := v.tagCache.Get(tag)
	if !ok {
		v.tagCache.lock.Lock()
		defer v.tagCache.lock.Unlock()

		// could have been multiple trying to access, but once first is done this ensures tag
		// isn't parsed again.
		ctag, ok = v.tagCache.Get(tag)
		if !ok {
			ctag, _ = v.parseFieldTagsRecursive(tag, "", "", false)
			v.tagCache.Set(tag, ctag)
		}
	}

	otherVal := reflect.ValueOf(other)

	vd := v.pool.Get().(*validate)
	vd.top = otherVal
	vd.isPartial = false

	vd.traverseField(otherVal, reflect.ValueOf(field), vd.ns[0:0], vd.actualNs[0:0], defaultCField, ctag)

	if len(vd.errs) > 0 {
		err = vd.errs
		vd.errs = nil
	}

	v.pool.Put(vd)

	return
}