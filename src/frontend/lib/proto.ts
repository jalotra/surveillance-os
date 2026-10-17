// Binary envelope codec — identical wire format to internal/proto on the
// hub and agent: [u32 BE header_len][header JSON][payload bytes].
// Each WebSocket binary message is exactly one envelope.

import type { WireHeader } from "./types";

export interface Envelope {
  header: WireHeader;
  payload: Uint8Array;
}

export function decodeEnvelope(buf: ArrayBuffer): Envelope | null {
  if (buf.byteLength < 4) return null;
  const dv = new DataView(buf);
  const hLen = dv.getUint32(0, false); // big-endian
  if (hLen <= 0 || 4 + hLen > buf.byteLength) return null;
  const headerBytes = new Uint8Array(buf, 4, hLen);
  let header: WireHeader;
  try {
    header = JSON.parse(new TextDecoder().decode(headerBytes));
  } catch {
    return null;
  }
  return { header, payload: new Uint8Array(buf, 4 + hLen) };
}
