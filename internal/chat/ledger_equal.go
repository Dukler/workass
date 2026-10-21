package chat

import (
	"reflect"
	"unsafe"
)

// ledgerRowEqual reports whether two ledger rows are equal in every field at
// every depth, with reflect.DeepEqual's answer for these types: nil and empty
// slices differ, as they do in the stored JSON.
//
// The SQLite store compares every row at every checkpoint, so the comparison
// is compiled once from the types and runs over memory: adjacent scalar and
// string fields as one block (exactLedgerCopy shares strings, so an
// unchanged string is an identical header and costs no byte reads), with no
// reflect.Value, allocation or visited set per node. On a 4,000-row, 443 MiB
// actor it compares every row in ~1.1 ms; reflect.DeepEqual took ~10.4 ms
// and allocated 8 MB (BenchmarkSQLLedgerChangeDetection).
func ledgerRowEqual(a, b *LedgerEvent) bool {
	return ledgerEventEqualPlan.equal(unsafe.Pointer(a), unsafe.Pointer(b))
}

var ledgerEventEqualPlan = compileExactEqual(reflect.TypeFor[LedgerEvent]())

type exactEqualOpKind uint8

const (
	// exactEqualBytes compares size bytes at offset: adjacent scalar fields,
	// never padding.
	exactEqualBytes exactEqualOpKind = iota
	// exactEqualString compares one string by content; it appears only as a
	// part of a run.
	exactEqualString
	// exactEqualRun compares adjacent scalar and string fields as one block
	// of memory: equal string headers mean equal strings. Only when the block
	// differs are its parts compared one by one, strings by content.
	exactEqualRun
	exactEqualPointer
	exactEqualSlice
	// exactEqualDeep falls back to reflect.DeepEqual (maps, interfaces and
	// other kinds a ledger row does not use today).
	exactEqualDeep
)

type exactEqualOp struct {
	kind   exactEqualOpKind
	offset uintptr
	size   uintptr
	elem   *exactEqualPlan // pointer and slice element
	typ    reflect.Type    // exactEqualDeep
	parts  []exactEqualOp  // exactEqualRun: its bytes and string fields
}

// exactEqualPlan compares two values of one type.
type exactEqualPlan struct {
	size uintptr
	ops  []exactEqualOp
	// flat: the type has no references and no padding, so an array of it
	// compares as one block of memory.
	flat bool
}

func compileExactEqual(typ reflect.Type) *exactEqualPlan {
	return newExactEqualCompiler().plan(typ)
}

type exactEqualCompiler struct {
	plans map[reflect.Type]*exactEqualPlan
}

func newExactEqualCompiler() *exactEqualCompiler {
	return &exactEqualCompiler{plans: make(map[reflect.Type]*exactEqualPlan)}
}

func (c *exactEqualCompiler) plan(typ reflect.Type) *exactEqualPlan {
	if plan, ok := c.plans[typ]; ok {
		return plan // a recursive type reuses the plan being built
	}
	plan := &exactEqualPlan{size: typ.Size()}
	c.plans[typ] = plan
	leaves := c.appendOps(nil, typ, 0)
	plan.flat = len(leaves) == 1 && leaves[0].kind == exactEqualBytes && leaves[0].size == plan.size
	plan.ops = mergeExactEqualRuns(leaves)
	return plan
}

// mergeExactEqualRuns turns each stretch of adjacent bytes and string ops
// (no padding between them) into one exactEqualRun.
func mergeExactEqualRuns(leaves []exactEqualOp) []exactEqualOp {
	var ops []exactEqualOp
	for index := 0; index < len(leaves); {
		leaf := leaves[index]
		if leaf.kind != exactEqualBytes && leaf.kind != exactEqualString {
			ops = append(ops, leaf)
			index++
			continue
		}
		run := exactEqualOp{kind: exactEqualRun, offset: leaf.offset}
		end := leaf.offset
		for index < len(leaves) && (leaves[index].kind == exactEqualBytes || leaves[index].kind == exactEqualString) && leaves[index].offset == end {
			part := leaves[index]
			if part.kind == exactEqualString {
				part.size = unsafe.Sizeof("")
			}
			run.parts = append(run.parts, part)
			end += part.size
			index++
		}
		run.size = end - run.offset
		if len(run.parts) == 1 && run.parts[0].kind == exactEqualBytes {
			run = run.parts[0]
		}
		ops = append(ops, run)
	}
	return ops
}

func (c *exactEqualCompiler) appendOps(ops []exactEqualOp, typ reflect.Type, offset uintptr) []exactEqualOp {
	switch typ.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		// Floats by bits, as their JSON tells them apart (-0 encodes
		// differently from 0; NaN does not encode at all).
		if typ.Size() == 0 {
			return ops
		}
		if last := len(ops) - 1; last >= 0 && ops[last].kind == exactEqualBytes && ops[last].offset+ops[last].size == offset {
			ops[last].size += typ.Size()
			return ops
		}
		return append(ops, exactEqualOp{kind: exactEqualBytes, offset: offset, size: typ.Size()})
	case reflect.String:
		return append(ops, exactEqualOp{kind: exactEqualString, offset: offset})
	case reflect.Pointer:
		return append(ops, exactEqualOp{kind: exactEqualPointer, offset: offset, elem: c.plan(typ.Elem())})
	case reflect.Slice:
		return append(ops, exactEqualOp{kind: exactEqualSlice, offset: offset, elem: c.plan(typ.Elem())})
	case reflect.Struct:
		for index := 0; index < typ.NumField(); index++ {
			field := typ.Field(index)
			ops = c.appendOps(ops, field.Type, offset+field.Offset)
		}
		return ops
	case reflect.Array:
		for index := 0; index < typ.Len(); index++ {
			ops = c.appendOps(ops, typ.Elem(), offset+uintptr(index)*typ.Elem().Size())
		}
		return ops
	default:
		return append(ops, exactEqualOp{kind: exactEqualDeep, offset: offset, typ: typ})
	}
}

// sliceHeader is the runtime layout of every slice.
type sliceHeader struct {
	data unsafe.Pointer
	len  int
	cap  int
}

func (p *exactEqualPlan) equal(a, b unsafe.Pointer) bool {
	if a == b {
		return true
	}
	for index := range p.ops {
		op := &p.ops[index]
		x, y := unsafe.Add(a, op.offset), unsafe.Add(b, op.offset)
		switch op.kind {
		case exactEqualRun:
			if unsafe.String((*byte)(x), op.size) == unsafe.String((*byte)(y), op.size) {
				continue
			}
			for part := range op.parts {
				if !op.parts[part].equalLeaf(a, b) {
					return false
				}
			}
		case exactEqualBytes:
			if unsafe.String((*byte)(x), op.size) != unsafe.String((*byte)(y), op.size) {
				return false
			}
		case exactEqualPointer:
			px, py := *(*unsafe.Pointer)(x), *(*unsafe.Pointer)(y)
			if px == nil || py == nil {
				if px != py {
					return false
				}
				continue
			}
			if !op.elem.equal(px, py) {
				return false
			}
		case exactEqualSlice:
			sx, sy := (*sliceHeader)(x), (*sliceHeader)(y)
			if sx.len != sy.len || (sx.data == nil) != (sy.data == nil) {
				return false
			}
			if !op.elem.equalArray(sx.data, sy.data, sx.len) {
				return false
			}
		case exactEqualDeep:
			if !reflect.DeepEqual(reflect.NewAt(op.typ, x).Elem().Interface(), reflect.NewAt(op.typ, y).Elem().Interface()) {
				return false
			}
		}
	}
	return true
}

func (op *exactEqualOp) equalLeaf(a, b unsafe.Pointer) bool {
	x, y := unsafe.Add(a, op.offset), unsafe.Add(b, op.offset)
	if op.kind == exactEqualString {
		return *(*string)(x) == *(*string)(y)
	}
	return unsafe.String((*byte)(x), op.size) == unsafe.String((*byte)(y), op.size)
}

func (p *exactEqualPlan) equalArray(a, b unsafe.Pointer, length int) bool {
	if length == 0 || a == b || p.size == 0 {
		return true
	}
	if p.flat {
		size := p.size * uintptr(length)
		return unsafe.String((*byte)(a), size) == unsafe.String((*byte)(b), size)
	}
	for index := 0; index < length; index++ {
		offset := p.size * uintptr(index)
		if !p.equal(unsafe.Add(a, offset), unsafe.Add(b, offset)) {
			return false
		}
	}
	return true
}
