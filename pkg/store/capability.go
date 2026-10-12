package store

// Unwrapper is a RunStore that decorates another. The capability probes
// (the As* functions) look through it for every optional interface it does
// not carry itself, so a decorator never hides a capability of the store it
// wraps: a probe that answers nil degrades in silence.
type Unwrapper interface {
	Unwrap() RunStore
}

// capability finds the optional interface T on s or, through Unwrap, on the
// store s decorates. A decorator that carries T itself answers first.
func capability[T any](s RunStore) T {
	for s != nil {
		if t, ok := s.(T); ok {
			return t
		}
		u, ok := s.(Unwrapper)
		if !ok {
			break
		}
		s = u.Unwrap()
	}
	var none T
	return none
}
