package parser

// parseLenient is the reading this package's tests take of a source, refused
// whole or not: parseAny on the budget Parse reads on. Unexported and in a
// test file: no exported door hands a refused reading over.
func parseLenient(filename, src string) *ParseResult {
	return parseAny(filename, src, sourceBudget())
}
