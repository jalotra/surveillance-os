// Minimal MP4 box walker — enough to pull the codec string out of an fMP4
// init segment so we can build the SourceBuffer MIME. Not a parser:
// just size/type traversal into moov→trak→mdia→minf→stbl→stsd→avc1→avcC.

function boxType(b: Uint8Array, off: number): string {
  return String.fromCharCode(b[off + 4], b[off + 5], b[off + 6], b[off + 7]);
}

function boxSize(b: Uint8Array, off: number): number {
  return new DataView(b.buffer, b.byteOffset + off, 8).getUint32(0, false);
}

// iterate top-level boxes inside a byte range, calling fn(type, bodyOff, boxEnd)
function walkBoxes(b: Uint8Array, start: number, end: number, fn: (t: string, body: number, end: number) => void) {
  let p = start;
  while (p + 8 <= end) {
    const size = boxSize(b, p);
    if (size < 8 || p + size > end + 8) break; // tolerate trailing slop
    fn(boxType(b, p), p + 8, Math.min(p + size, end));
    p += size;
  }
}

function findBox(b: Uint8Array, start: number, end: number, want: string): [number, number] | null {
  let found: [number, number] | null = null;
  walkBoxes(b, start, end, (t, body, boxEnd) => {
    if (t === want && !found) found = [body, boxEnd];
  });
  return found;
}

// codecFromInit extracts e.g. "avc1.640028" from an init segment (ftyp+moov).
// Falls back to bare "avc1" when the box layout is unexpected.
export function codecFromInit(init: Uint8Array): string {
  try {
    const moov = findBox(init, 0, init.length, "moov");
    if (!moov) return "avc1";
    const trak = findBox(init, moov[0], moov[1], "trak");
    if (!trak) return "avc1";
    const mdia = findBox(init, trak[0], trak[1], "mdia");
    if (!mdia) return "avc1";
    const minf = findBox(init, mdia[0], mdia[1], "minf");
    if (!minf) return "avc1";
    const stbl = findBox(init, minf[0], minf[1], "stbl");
    if (!stbl) return "avc1";
    const stsd = findBox(init, stbl[0], stbl[1], "stsd");
    if (!stsd) return "avc1";
    // stsd body: u32 ver/flags, u32 entry_count, then sample entries.
    // The avc1 visual entry embeds avcC at entry_body + 78 + 8 (header).
    const entry = stsd[0] + 8;
    if (boxType(init, entry) !== "avc1") return "avc1";
    const avcC = findBox(init, entry + 8 + 78, boxSize(init, entry) + entry, "avcC");
    if (!avcC) return "avc1";
    // avcC payload: version, profile, compat, level → hex of bytes 1..3
    const hex = (i: number) => init[avcC[0] + i].toString(16).padStart(2, "0");
    return `avc1.${hex(1)}${hex(2)}${hex(3)}`;
  } catch {
    return "avc1";
  }
}
