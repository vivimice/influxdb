package reads

func Modulo(dividend, modulus int64) int64 {
	r := dividend % modulus
	if r < 0 {
		r += modulus
	}
	return r
}
