package reads

func Modulo(dividend, modulus int64) int64 {
	r := dividend % modulus
	if r < 0 {
		r += modulus
	}
	return r
}

func WindowStart(t, every, offset int64) int64 {
	mod := Modulo(t, every)
	off := Modulo(offset, every)
	beg := t - mod + off
	if mod < off {
		beg -= every
	}
	return beg
}

func WindowStop(t, every, offset int64) int64 {
	mod := Modulo(t, every)
	off := Modulo(offset, every)
	end := t - mod + off
	if mod >= off {
		end += every
	}
	return end
}
