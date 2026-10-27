package storekit

import "reflect"

// Snapshot copies an acyclic store document's mutable exported fields. Store
// readers and writers own their snapshots: a failed CAS cannot mutate a map,
// slice or pointer retained by the memory store. Scalar structs such as time.Time
// keep their unexported immutable representation, including nanosecond precision.
func Snapshot[T any](value T) T {
	return snapshotValue(reflect.ValueOf(value)).Interface().(T)
}

func snapshotValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(snapshotValue(v.Elem()))
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(snapshotValue(v.Index(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		it := v.MapRange()
		for it.Next() {
			out.SetMapIndex(it.Key(), snapshotValue(it.Value()))
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if out.Field(i).CanSet() && v.Field(i).CanInterface() {
				out.Field(i).Set(snapshotValue(v.Field(i)))
			}
		}
		return out
	default:
		return v
	}
}
