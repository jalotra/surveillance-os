// Command fakecam is a dev stand-in for gst-launch-1.0: it writes an
// fMP4 stream to stdout — init segment once, then moof+mdat fragments —
// so the real agent↔hub path can be exercised on a machine with no
// camera or GStreamer. Used by scripts/e2e-local.sh via a shim named
// "gst-launch-1.0" on PATH.
//
//	fakecam -fps 10 -mdat 2048          # stream forever
//	fakecam -frags 30                   # emit 30 fragments then exit
//
// Boxes are structurally valid (the splitter's keyframe walk reads real
// tfhd default_sample_flags) but are NOT decodable — no browser should
// ever see this stream.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"time"
)

func box(typ string, body []byte) []byte {
	b := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:], typ)
	copy(b[8:], body)
	return b
}

func initSegment() []byte {
	ftyp := box("ftyp", append([]byte("isom"), 0, 0, 2, 0, 'i', 's', 'o', 'm', 'a', 'v', 'c', '1', 'm', 'p', '4', '1'))
	moov := box("moov", make([]byte, 64)) // placeholder; decoder not needed
	return append(ftyp, moov...)
}

// fragment builds moof{mfhd, traf{tfhd}} + mdat{payload}.
// key=true → default_sample_flags without the non-sync bit.
func fragment(seq uint64, key bool, payloadLen int, rng *rand.Rand) []byte {
	mfhdBody := make([]byte, 8)
	binary.BigEndian.PutUint32(mfhdBody[4:], uint32(seq))
	mfhd := box("mfhd", mfhdBody)

	var flags uint32 = 0x00010000 // sample_is_non_sync_sample → delta frame
	if key {
		flags = 0x02000000
	}
	tfhdBody := make([]byte, 12)
	binary.BigEndian.PutUint32(tfhdBody[0:], 0x000020) // default_sample_flags present
	binary.BigEndian.PutUint32(tfhdBody[4:], 1)      // track_ID
	binary.BigEndian.PutUint32(tfhdBody[8:], flags)
	traf := box("traf", box("tfhd", tfhdBody))

	moofBody := append(mfhd, traf...)
	moof := box("moof", moofBody)

	payload := make([]byte, payloadLen)
	rng.Read(payload)
	mdat := box("mdat", payload)

	return append(moof, mdat...)
}

func main() {
	fps := flag.Float64("fps", 10, "fragments per second")
	gop := flag.Int("gop", 2, "keyframe every N fragments")
	n := flag.Int("frags", 0, "stop after N fragments (0 = forever)")
	mdat := flag.Int("mdat", 4096, "mdat payload bytes")
	flag.Parse()

	rng := rand.New(rand.NewSource(1))
	out := os.Stdout
	if _, err := out.Write(initSegment()); err != nil {
		return // broken pipe — agent died
	}
	var i uint64
	for *n == 0 || i < uint64(*n) {
		i++
		if _, err := out.Write(fragment(i, i%uint64(*gop) == 1, *mdat, rng)); err != nil {
			return
		}
		if *fps > 0 {
			time.Sleep(time.Duration(float64(time.Second) / *fps))
		}
	}
	fmt.Fprintln(os.Stderr, "fakecam: done")
}
