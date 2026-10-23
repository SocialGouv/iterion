package unit

// salvageUnit is the unit a salvage views — a whole load's, or the files a
// load cut short read — for this package's tests, which read what the view
// hands no one else.
func salvageUnit(u *Unit, err error) *Unit {
	return Salvage(u, err).u
}
