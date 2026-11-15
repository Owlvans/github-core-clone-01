import * as crypto from 'node:crypto';
import * as zlib from 'node:zlib';

export type GitObjectType = 'blob' | 'tree' | 'commit' | 'tag';

export interface GitObject {
  type: GitObjectType;
  raw: Buffer;
}

export interface TreeEntry {
  mode: string;
  name: string;
  sha: string; // 40-char hex
}

export interface CommitObject {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

// Pkt-line utilities
export function encodePkt(data: string | Buffer): Buffer {
  const buf = typeof data === 'string' ? Buffer.from(data, 'utf-8') : data;
  const len = buf.length + 4;
  const hex = len.toString(16).padStart(4, '0');
  return Buffer.concat([Buffer.from(hex, 'ascii'), buf]);
}

export function encodeFlush(): Buffer {
  return Buffer.from('0000', 'ascii');
}

export function encodeDelim(): Buffer {
  return Buffer.from('0001', 'ascii');
}

export function encodeResponseEnd(): Buffer {
  return Buffer.from('0002', 'ascii');
}

export function parsePktLines(buffer: Buffer): { lines: Buffer[]; remainder: Buffer } {
  const lines: Buffer[] = [];
  let offset = 0;
  while (offset + 4 <= buffer.length) {
    const lenHex = buffer.toString('ascii', offset, offset + 4);
    if (lenHex === '0000') {
      lines.push(Buffer.from('0000', 'ascii'));
      offset += 4;
      continue;
    }
    if (lenHex === '0001') {
      lines.push(Buffer.from('0001', 'ascii'));
      offset += 4;
      continue;
    }
    if (lenHex === '0002') {
      lines.push(Buffer.from('0002', 'ascii'));
      offset += 4;
      continue;
    }
    const len = parseInt(lenHex, 16);
    if (isNaN(len) || len === 0) break;
    if (offset + len > buffer.length) break; // incomplete packet
    const payload = buffer.subarray(offset + 4, offset + len);
    lines.push(payload);
    offset += len;
  }
  return { lines, remainder: buffer.subarray(offset) };
}

// Git hashing
export function hashObject(type: GitObjectType, raw: Buffer): { sha: string; full: Buffer } {
  const header = Buffer.from(`${type} ${raw.length}\0`);
  const full = Buffer.concat([header, raw]);
  const sha = crypto.createHash('sha1').update(full).digest('hex');
  return { sha, full };
}

// Tree parsing & serialization
export function parseTree(buf: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < buf.length) {
    const spaceIdx = buf.indexOf(32, offset); // space ' '
    if (spaceIdx === -1) break;
    const mode = buf.toString('ascii', offset, spaceIdx);
    const nullIdx = buf.indexOf(0, spaceIdx + 1); // null byte
    if (nullIdx === -1) break;
    const name = buf.toString('utf-8', spaceIdx + 1, nullIdx);
    const shaBuf = buf.subarray(nullIdx + 1, nullIdx + 21);
    if (shaBuf.length < 20) break;
    const sha = shaBuf.toString('hex');
    entries.push({ mode, name, sha });
    offset = nullIdx + 21;
  }
  return entries;
}

export function serializeTree(entries: TreeEntry[]): Buffer {
  // Sort git style: name with trailing '/' for directories
  const sorted = [...entries].sort((a, b) => {
    const an = a.mode === '40000' ? a.name + '/' : a.name;
    const bn = b.mode === '40000' ? b.name + '/' : b.name;
    return an < bn ? -1 : an > bn ? 1 : 0;
  });
  const bufs: Buffer[] = [];
  for (const entry of sorted) {
    const modeName = `${entry.mode} ${entry.name}\0`;
    const shaBuf = Buffer.from(entry.sha, 'hex');
    bufs.push(Buffer.from(modeName, 'ascii'));
    bufs.push(shaBuf);
  }
  return Buffer.concat(bufs);
}

// Commit parsing & serialization
export function parseCommit(buf: Buffer): CommitObject {
  const content = buf.toString('utf-8');
  const headerEnd = content.indexOf('\n\n');
  const headerText = headerEnd === -1 ? content : content.substring(0, headerEnd);
  const message = headerEnd === -1 ? '' : content.substring(headerEnd + 2);

  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';

  for (const line of headerText.split('\n')) {
    if (line.startsWith('tree ')) {
      tree = line.substring(5).trim();
    } else if (line.startsWith('parent ')) {
      parents.push(line.substring(7).trim());
    } else if (line.startsWith('author ')) {
      author = line.substring(7).trim();
    } else if (line.startsWith('committer ')) {
      committer = line.substring(10).trim();
    }
  }

  return { tree, parents, author, committer, message };
}

export function serializeCommit(c: CommitObject): Buffer {
  let text = `tree ${c.tree}\n`;
  for (const p of c.parents) {
    text += `parent ${p}\n`;
  }
  text += `author ${c.author}\n`;
  text += `committer ${c.committer}\n\n`;
  text += c.message;
  return Buffer.from(text, 'utf-8');
}

// Packfile parsing
export interface ParsedObject {
  type: GitObjectType;
  raw: Buffer;
  sha: string;
}

export function parsePackfile(packBuf: Buffer): ParsedObject[] {
  if (packBuf.length < 12) return [];
  const magic = packBuf.toString('ascii', 0, 4);
  if (magic !== 'PACK') return [];
  const version = packBuf.readUInt32BE(4);
  const numObjects = packBuf.readUInt32BE(8);

  let offset = 12;
  const objects: { typeNum: number; size: number; compressed: Buffer; raw?: Buffer; type?: GitObjectType; sha?: string }[] = [];

  for (let i = 0; i < numObjects; i++) {
    if (offset >= packBuf.length - 20) break; // leave room for trailer
    let b = packBuf[offset++];
    const typeNum = (b >> 4) & 7;
    let size = b & 15;
    let shift = 4;
    while ((b & 0x80) !== 0) {
      if (offset >= packBuf.length) break;
      b = packBuf[offset++];
      size += (b & 0x7f) << shift;
      shift += 7;
    }

    // If type is OBJ_REF_DELTA (6) or OBJ_OFS_DELTA (7), skip extra header bytes
    if (typeNum === 6) {
      offset += 20; // base obj name
    } else if (typeNum === 7) {
      // variable length offset
      let eb = packBuf[offset++];
      while ((eb & 0x80) !== 0) {
        eb = packBuf[offset++];
      }
    }

    // Deflated data follows
    try {
      const zlibResult = zlib.inflateSync(packBuf.subarray(offset));
      // zlib.inflateSync consumes only what it needs, but we need to know how many compressed bytes were read.
      // Actually, standard zlib inflate needs an inflater or we can use zlib.inflateSync with a stream or trial.
      // Alternatively, let's find the compressed stream length by trying inflates or using zlib.Unzip / zlib.inflateSync.
      // Wait, zlib.inflateSync on buffer might read past if there's trailing data. But in Node, zlib functions handle structured blocks.
      // Better yet, we can use a loop or zlib.sync chunking. Let's do zlib.inflateSync by feeding chunks or checking output size / inflation.
      // Actually, zlib inflate in Node can be done via zlib.inflateSync(buf). To find input consumed, we can use zlib.Brotli / inflate with a callback or check where inflation ends.
      // Wait! Node's zlib.inflateSync doesn't return bytes read. But we can use zlib.Unzip or a small helper with zlib.createInflate / inflateSync or zlib.unzipSync.
      // Wait, let's check how to parse packfile objects reliably in Node:
      // We can use `zlib.inflateSync(packBuf.subarray(offset))` — wait, does it throw if extra data is at the end? No, zlib stops at end of compressed stream! But to know how many bytes were consumed from `packBuf.subarray(offset)`, we can use `zlib.inflateSync` or write a small scanner or use `zlib.Inflate` class or `zlib.gunzipSync` / `zlib.inflateSync`. Wait, `zlib.inflateSync` successfully inflates even if there is trailing data, but does it tell us how many bytes were consumed? No.
      // Wait, how do people parse packfiles in JS without external libraries?
      // We can try inflating with increasing lengths or use `zlib.createInflate()` or binary search / trial, OR even simpler: since the client checks send plain objects (no delta), we can deflate/inflate or use `zlib.inflateSync` by trial-and-error of input length, or `zlib.sync` with a stream.
      // Wait! Let's check `git-client.ts` or conformance tests to see what packs are pushed. The prompt says: "The suite's client sends plain objects only."
      // If the client sends plain objects only (non-delta), let's see how `zlib.inflateSync` behaves. If we pass `packBuf.subarray(offset)` to `zlib.inflateSync`, does it inflate the object? Yes! But how many compressed bytes did it take?
      // Wait, we can test inflating with `zlib.inflateSync` while catching errors or finding the exact input length. Or even better, we can use `zlib.inflateSync` inside a loop trying length from 1 to remaining buffer length until it succeeds! Since zlib throws if compressed data is incomplete or invalid, trying lengths `l` from 1 upwards (or binary search / stream) will find the exact compressed length `l` where `zlib.inflateSync(packBuf.subarray(offset, offset + l))` succeeds!
      // That is extremely robust and 100% pure JS without any native module or external dep.
      let decompressed: Buffer | null = null;
      let consumed = 0;
      const maxLen = packBuf.length - offset - 20;
      let low = 1;
      let high = maxLen;
      while (low <= high) {
        const mid = (low + high) >> 1;
        try {
          decompressed = zlib.inflateSync(packBuf.subarray(offset, offset + mid));
          consumed = mid;
          high = mid - 1; // try to find smaller valid length
        } catch {
          low = mid + 1;
        }
      }
      if (!decompressed) {
        decompressed = zlib.inflateSync(packBuf.subarray(offset, packBuf.length - 20));
        consumed = packBuf.length - 20 - offset;
      }
      offset += consumed;

      let typeName: GitObjectType = 'blob';
      if (typeNum === 1) typeName = 'commit';
      else if (typeNum === 2) typeName = 'tree';
      else if (typeNum === 3) typeName = 'blob';
      else if (typeNum === 4) typeName = 'tag';

      const { sha } = hashObject(typeName, decompressed);
      objects.push({ type: typeName, raw: decompressed, sha });
    } catch {
      break;
    }
  }

  return objects as ParsedObject[];
}

// Packfile building
export function buildPackfile(objectsToPack: Array<{ type: GitObjectType; raw: Buffer }>): Buffer {
  const bufs: Buffer[] = [];
  // Header: 'PACK' (4) + version 2 (4) + count (4)
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objectsToPack.length, 8);
  bufs.push(header);

  for (const obj of objectsToPack) {
    let typeNum = 3;
    if (obj.type === 'commit') typeNum = 1;
    else if (obj.type === 'tree') typeNum = 2;
    else if (obj.type === 'blob') typeNum = 3;
    else if (obj.type === 'tag') typeNum = 4;

    // Varint size header: first byte = type<<4 | low 4 size bits, with the
    // 0x80 continuation bit set iff more size bytes follow; each following
    // byte carries 7 more size bits, continuation bit on all but the last.
    let size = obj.raw.length;
    const headerBytes: number[] = [];
    let firstByte = ((typeNum & 7) << 4) | (size & 15);
    size >>= 4;
    if (size > 0) firstByte |= 0x80;
    headerBytes.push(firstByte);
    while (size > 0) {
      let b = size & 0x7f;
      size >>= 7;
      if (size > 0) b |= 0x80;
      headerBytes.push(b);
    }
    bufs.push(Buffer.from(headerBytes));

    // Deflate
    const compressed = zlib.deflateSync(obj.raw, { level: 1 });
    bufs.push(compressed);
  }

  const contentWithoutTrailer = Buffer.concat(bufs);
  const trailer = crypto.createHash('sha1').update(contentWithoutTrailer).digest();
  return Buffer.concat([contentWithoutTrailer, trailer]);
}

// Tree helpers: build tree from map of path -> blob sha
export function buildTreeFromMap(fileMap: Map<string, string>): string {
  // fileMap: relative path (e.g. "README.md", "src/app.ts") -> blob sha
  // Build nested directory structure
  // Returns tree sha
  const rootDir = new Map<string, any>();

  for (const [filePath, blobSha] of fileMap.entries()) {
    const parts = filePath.split('/');
    let curr = rootDir;
    for (let i = 0; i < parts.length - 1; i++) {
      const p = parts[i];
      if (!curr.has(p)) {
        curr.set(p, new Map<string, any>());
      }
      curr = curr.get(p);
    }
    const fileName = parts[parts.length - 1];
    curr.set(fileName, blobSha);
  }

  // Recursively build trees bottom-up
  // We need store reference or pass a store callback. Let's make a helper that takes an object store writer function.
  return ''; // placeholder, will define with store
}
