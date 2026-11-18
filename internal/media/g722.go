package media

import "math"

// G.722 (ITU-T, sub-band ADPCM at 64 kbit/s, 16 kHz audio) in pure Go, written to be bit-exact with the
// reference encoder and decoder (checked against libspandsp's output in g722_test.go).
//
// RTP quirk (RFC 3551): G.722 samples at 16 kHz but its RTP clock is 8000 Hz, so a 20 ms packet is 160 bytes,
// carries 320 samples and advances the timestamp by 160 -- exactly like a 20 ms G.711 packet. The rest of the
// switch works on 8 kHz frames, so G722Decoder.Decode8k and G722Encoder.Encode8k resample at the edge.

var g722QMF = [12]int{3, -11, 12, 32, -210, 951, 3876, -805, 362, -156, 53, -11}

var g722Q6 = [32]int{0, 35, 72, 110, 150, 190, 233, 276, 323, 370, 422, 473, 530, 587, 650, 714,
	786, 858, 940, 1023, 1121, 1219, 1339, 1458, 1612, 1765, 1980, 2195, 2557, 2919, 0, 0}
var g722ILN = [32]int{0, 63, 62, 31, 30, 29, 28, 27, 26, 25, 24, 23, 22, 21, 20, 19,
	18, 17, 16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 0}
var g722ILP = [32]int{0, 61, 60, 59, 58, 57, 56, 55, 54, 53, 52, 51, 50, 49, 48, 47,
	46, 45, 44, 43, 42, 41, 40, 39, 38, 37, 36, 35, 34, 33, 32, 0}
var g722WL = [8]int{-60, -30, 58, 172, 334, 538, 1198, 3042}
var g722RL42 = [16]int{0, 7, 6, 5, 4, 3, 2, 1, 7, 6, 5, 4, 3, 2, 1, 0}
var g722ILB = [32]int{2048, 2093, 2139, 2186, 2233, 2282, 2332, 2383, 2435, 2489, 2543, 2599, 2656, 2714, 2774, 2834,
	2896, 2960, 3025, 3091, 3158, 3228, 3298, 3371, 3444, 3520, 3597, 3676, 3756, 3838, 3922, 4008}
var g722QM4 = [16]int{0, -20456, -12896, -8968, -6288, -4240, -2584, -1200,
	20456, 12896, 8968, 6288, 4240, 2584, 1200, 0}
var g722QM2 = [4]int{-7408, -1616, 7408, 1616}
var g722QM6 = [64]int{-136, -136, -136, -136, -24808, -21904, -19008, -16704, -14984, -13512, -12280, -11192,
	-10232, -9360, -8576, -7856, -7192, -6576, -6000, -5456, -4944, -4464, -4008, -3576, -3168, -2776,
	-2400, -2032, -1688, -1360, -1040, -728, 24808, 21904, 19008, 16704, 14984, 13512, 12280, 11192,
	10232, 9360, 8576, 7856, 7192, 6576, 6000, 5456, 4944, 4464, 4008, 3576, 3168, 2776, 2400, 2032,
	1688, 1360, 1040, 728, 432, 136, -432, -136}
var g722IHN = [3]int{0, 1, 0}
var g722IHP = [3]int{0, 3, 2}
var g722WH = [3]int{0, -214, 798}
var g722RH2 = [4]int{2, 1, 2, 1}

func g722Sat(v int) int {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return v
}

// g722Band is the adaptive predictor and quantiser state of one sub-band.
type g722Band struct {
	s, sp, sz int
	r, a, ap  [3]int
	p         [3]int
	d, b, bp  [7]int
	sg        [7]int
	nb, det   int
}

// block4 updates the predictor with the quantised difference signal d.
func (b *g722Band) block4(d int) {
	b.d[0] = d
	b.r[0] = g722Sat(b.s + d)
	b.p[0] = g722Sat(b.sz + d)

	for i := 0; i < 3; i++ {
		b.sg[i] = b.p[i] >> 15
	}
	wd1 := g722Sat(b.a[1] << 2)
	wd2 := wd1
	if b.sg[0] == b.sg[1] {
		wd2 = -wd1
	}
	if wd2 > 32767 {
		wd2 = 32767
	}
	wd3 := wd2 >> 7
	if b.sg[0] == b.sg[2] {
		wd3 += 128
	} else {
		wd3 -= 128
	}
	wd3 += (b.a[2] * 32512) >> 15
	if wd3 > 12288 {
		wd3 = 12288
	} else if wd3 < -12288 {
		wd3 = -12288
	}
	b.ap[2] = wd3

	b.sg[0] = b.p[0] >> 15
	b.sg[1] = b.p[1] >> 15
	if b.sg[0] == b.sg[1] {
		wd1 = 192
	} else {
		wd1 = -192
	}
	wd2 = (b.a[1] * 32640) >> 15
	b.ap[1] = g722Sat(wd1 + wd2)
	wd3 = g722Sat(15360 - b.ap[2])
	if b.ap[1] > wd3 {
		b.ap[1] = wd3
	} else if b.ap[1] < -wd3 {
		b.ap[1] = -wd3
	}

	if d == 0 {
		wd1 = 0
	} else {
		wd1 = 128
	}
	b.sg[0] = d >> 15
	for i := 1; i < 7; i++ {
		b.sg[i] = b.d[i] >> 15
		if b.sg[i] == b.sg[0] {
			wd2 = wd1
		} else {
			wd2 = -wd1
		}
		wd3 = (b.b[i] * 32640) >> 15
		b.bp[i] = g722Sat(wd2 + wd3)
	}

	for i := 6; i > 0; i-- {
		b.d[i] = b.d[i-1]
		b.b[i] = b.bp[i]
	}
	for i := 2; i > 0; i-- {
		b.r[i] = b.r[i-1]
		b.p[i] = b.p[i-1]
		b.a[i] = b.ap[i]
	}

	wd1 = g722Sat(b.r[1] + b.r[1])
	wd1 = (b.a[1] * wd1) >> 15
	wd2 = g722Sat(b.r[2] + b.r[2])
	wd2 = (b.a[2] * wd2) >> 15
	b.sp = g722Sat(wd1 + wd2)

	b.sz = 0
	for i := 6; i > 0; i-- {
		wd1 = g722Sat(b.d[i] + b.d[i])
		b.sz += (b.b[i] * wd1) >> 15
	}
	b.sz = g722Sat(b.sz)

	b.s = g722Sat(b.sp + b.sz)
}

func g722Scale(nb, bias int) int {
	wd1 := (nb >> 6) & 31
	wd2 := bias - (nb >> 11)
	var wd3 int
	if wd2 < 0 {
		wd3 = g722ILB[wd1] << uint(-wd2)
	} else {
		wd3 = g722ILB[wd1] >> uint(wd2)
	}
	return wd3 << 2
}

// G722Encoder is one direction of a G.722 stream (64 kbit/s). The zero value is not ready: use NewG722Encoder.
type G722Encoder struct {
	x    [24]int
	band [2]g722Band
	up   upsampler
}

func NewG722Encoder() *G722Encoder {
	e := &G722Encoder{}
	e.band[0].det = 32
	e.band[1].det = 8
	return e
}

// Encode turns 16 kHz samples (an even count) into G.722 bytes, one byte per two samples, appended to dst.
func (e *G722Encoder) Encode(dst []byte, pcm []int16) []byte {
	for j := 0; j+1 < len(pcm); j += 2 {
		copy(e.x[:22], e.x[2:])
		e.x[22], e.x[23] = int(pcm[j]), int(pcm[j+1])
		var sumeven, sumodd int
		for i := 0; i < 12; i++ {
			sumodd += e.x[2*i] * g722QMF[i]
			sumeven += e.x[2*i+1] * g722QMF[11-i]
		}
		xlow := (sumeven + sumodd) >> 14
		xhigh := (sumeven - sumodd) >> 14

		el := g722Sat(xlow - e.band[0].s)
		wd := el
		if el < 0 {
			wd = -(el + 1)
		}
		i := 1
		for ; i < 30; i++ {
			if wd < (g722Q6[i]*e.band[0].det)>>12 {
				break
			}
		}
		var ilow int
		if el < 0 {
			ilow = g722ILN[i]
		} else {
			ilow = g722ILP[i]
		}
		ril := ilow >> 2
		dlow := (e.band[0].det * g722QM4[ril]) >> 15
		il4 := g722RL42[ril]
		nb := (e.band[0].nb*127)>>7 + g722WL[il4]
		if nb < 0 {
			nb = 0
		} else if nb > 18432 {
			nb = 18432
		}
		e.band[0].nb = nb
		e.band[0].det = g722Scale(nb, 8)
		e.band[0].block4(dlow)

		eh := g722Sat(xhigh - e.band[1].s)
		wd = eh
		if eh < 0 {
			wd = -(eh + 1)
		}
		mih := 1
		if wd >= (564*e.band[1].det)>>12 {
			mih = 2
		}
		var ihigh int
		if eh < 0 {
			ihigh = g722IHN[mih]
		} else {
			ihigh = g722IHP[mih]
		}
		dhigh := (e.band[1].det * g722QM2[ihigh]) >> 15
		ih2 := g722RH2[ihigh]
		nbh := (e.band[1].nb*127)>>7 + g722WH[ih2]
		if nbh < 0 {
			nbh = 0
		} else if nbh > 22528 {
			nbh = 22528
		}
		e.band[1].nb = nbh
		e.band[1].det = g722Scale(nbh, 10)
		e.band[1].block4(dhigh)

		dst = append(dst, byte(ihigh<<6|ilow))
	}
	return dst
}

// G722Decoder is the receiving direction of a G.722 stream (64 kbit/s).
type G722Decoder struct {
	x    [24]int
	band [2]g722Band
	down downsampler
}

func NewG722Decoder() *G722Decoder {
	d := &G722Decoder{}
	d.band[0].det = 32
	d.band[1].det = 8
	return d
}

// Decode turns G.722 bytes into 16 kHz samples (two per byte), appended to dst.
func (d *G722Decoder) Decode(dst []int16, data []byte) []int16 {
	for _, code := range data {
		wd1 := int(code & 0x3F)
		ihigh := int(code>>6) & 3
		wd2 := g722QM6[wd1]
		wd1 >>= 2

		wd2 = (d.band[0].det * wd2) >> 15
		rlow := d.band[0].s + wd2
		if rlow > 16383 {
			rlow = 16383
		} else if rlow < -16384 {
			rlow = -16384
		}
		dlowt := (d.band[0].det * g722QM4[wd1]) >> 15
		nb := (d.band[0].nb*127)>>7 + g722WL[g722RL42[wd1]]
		if nb < 0 {
			nb = 0
		} else if nb > 18432 {
			nb = 18432
		}
		d.band[0].nb = nb
		d.band[0].det = g722Scale(nb, 8)
		d.band[0].block4(dlowt)

		dhigh := (d.band[1].det * g722QM2[ihigh]) >> 15
		rhigh := dhigh + d.band[1].s
		if rhigh > 16383 {
			rhigh = 16383
		} else if rhigh < -16384 {
			rhigh = -16384
		}
		nbh := (d.band[1].nb*127)>>7 + g722WH[g722RH2[ihigh]]
		if nbh < 0 {
			nbh = 0
		} else if nbh > 22528 {
			nbh = 22528
		}
		d.band[1].nb = nbh
		d.band[1].det = g722Scale(nbh, 10)
		d.band[1].block4(dhigh)

		copy(d.x[:22], d.x[2:])
		d.x[22], d.x[23] = rlow+rhigh, rlow-rhigh
		var xout1, xout2 int
		for i := 0; i < 12; i++ {
			xout2 += d.x[2*i] * g722QMF[i]
			xout1 += d.x[2*i+1] * g722QMF[11-i]
		}
		dst = append(dst, int16(g722Sat(xout1>>11)), int16(g722Sat(xout2>>11)))
	}
	return dst
}

// ---------------------------------------------------------------------------
// 8 kHz <-> 16 kHz

// resampleTaps is a 31-tap low-pass at 3.6 kHz (for 16 kHz audio), Hann windowed, in Q15 with unity gain.
var resampleTaps = func() [31]int {
	var t [31]int
	const n = 31
	fc := 3600.0 / 16000.0
	var sum float64
	var f [n]float64
	for i := range f {
		x := float64(i - n/2)
		var s float64
		if x == 0 {
			s = 2 * fc
		} else {
			s = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		f[i] = s * (0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1)))
		sum += f[i]
	}
	for i := range f {
		t[i] = int(math.Round(f[i] / sum * 32768))
	}
	return t
}()

// downsampler halves the rate (16 kHz to 8 kHz) with the low-pass above, keeping its history between calls.
type downsampler struct {
	hist [31]int
	odd  bool
}

func (d *downsampler) process(dst []int16, in []int16) []int16 {
	for _, v := range in {
		copy(d.hist[:30], d.hist[1:])
		d.hist[30] = int(v)
		d.odd = !d.odd
		if !d.odd { // every second input sample yields an output
			continue
		}
		var acc int
		for i, c := range resampleTaps {
			acc += c * d.hist[i]
		}
		dst = append(dst, int16(g722Sat(acc>>15)))
	}
	return dst
}

// upsampler doubles the rate (8 kHz to 16 kHz): zero-stuffing, then the low-pass at twice the gain.
type upsampler struct {
	hist [31]int
}

func (u *upsampler) process(dst []int16, in []int16) []int16 {
	for _, v := range in {
		for k := 0; k < 2; k++ {
			x := 0
			if k == 0 {
				x = int(v)
			}
			copy(u.hist[:30], u.hist[1:])
			u.hist[30] = x
			var acc int
			for i, c := range resampleTaps {
				acc += c * u.hist[i]
			}
			dst = append(dst, int16(g722Sat(2*acc>>15)))
		}
	}
	return dst
}

// Decode8k decodes G.722 bytes to 8 kHz samples (the rate the switch works at): 160 bytes in, 160 samples out.
func (d *G722Decoder) Decode8k(dst []int16, data []byte) []int16 {
	wide := d.Decode(nil, data)
	return d.down.process(dst, wide)
}

// Encode8k encodes 8 kHz samples (the rate the switch works at): 160 samples in, 160 bytes out.
func (e *G722Encoder) Encode8k(dst []byte, pcm []int16) []byte {
	wide := e.up.process(nil, pcm)
	return e.Encode(dst, wide)
}
