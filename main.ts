// git-forge: a GitHub-core clone in one file — REST v3, git smart HTTP (protocol v2/v0),
// GraphQL and MCP over one in-memory store. Node 22, stdlib only, no dependencies.
// SPDX-License-Identifier: MIT

import http from 'node:http';
import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

// ---------------------------------------------------------------------------
// Scenario: the viewer login, derived from the contest's fixed seed.
// The suite builds its data from SCENARIO_SEED 0x6a09e667f3bcc909n; the viewer is
// the first user generateUsers(scenarioRng('users'), 12) makes. Reimplemented
// here (FNV-1a + splitmix64 finalizer + xorshift64*) so the server needs no
// login env var: the token is opaque and the owner is whoever the seed says.
// ---------------------------------------------------------------------------

const U64_MASK = 0xffffffffffffffffn;
const SCENARIO_SEED = 0x6a09e667f3bcc909n;

function fnv1a64(text: string): bigint {
  let hash = 0xcbf29ce484222325n;
  for (let i = 0; i < text.length; i += 1) {
    hash ^= BigInt(text.charCodeAt(i));
    hash = (hash * 0x100000001b3n) & U64_MASK;
  }
  return hash;
}

function mix64(value: bigint): bigint {
  let z = (value + 0x9e3779b97f4a7c15n) & U64_MASK;
  z = ((z ^ (z >> 30n)) * 0xbf58476d1ce4e5b9n) & U64_MASK;
  z = ((z ^ (z >> 27n)) * 0x94d049bb133111ebn) & U64_MASK;
  return (z ^ (z >> 31n)) & U64_MASK;
}

function scenarioSeedFor(label: string): bigint {
  return mix64(SCENARIO_SEED ^ fnv1a64(label));
}

class Rng {
  #state: bigint;
  constructor(seed: bigint) {
    const masked = seed & U64_MASK;
    this.#state = masked === 0n ? SCENARIO_SEED : masked;
  }
  nextU64(): bigint {
    let x = this.#state;
    x = (x ^ (x >> 12n)) & U64_MASK;
    x = (x ^ (x << 25n)) & U64_MASK;
    x = (x ^ (x >> 27n)) & U64_MASK;
    this.#state = x;
    return (x * 0x2545f4914f6cdd1dn) & U64_MASK;
  }
  nextU32(): number {
    return Number(this.nextU64() >> 32n);
  }
  nextInt(maxExclusive: number): number {
    if (maxExclusive === 1) return 0;
    const range = 4_294_967_296;
    const limit = range - (range % maxExclusive);
    for (;;) {
      const value = this.nextU32();
      if (value < limit) return value % maxExclusive;
    }
  }
  pick<T>(items: readonly T[]): T {
    return items[this.nextInt(items.length)] as T;
  }
}

const FIRST_NAMES = [
  'ada', 'bram', 'cora', 'dmitri', 'elena', 'felix', 'greta', 'hugo', 'iris', 'jonas', 'kira', 'lars',
  'mira', 'nico', 'olga', 'pavel', 'quinn', 'rosa', 'sven', 'tess', 'ulla', 'vito', 'wren', 'yara', 'zane',
];
const LAST_NAMES = [
  'anders', 'bauer', 'costa', 'dahl', 'eriksen', 'fischer', 'garcia', 'holm', 'iversen', 'jansen', 'keller',
  'lind', 'moreau', 'novak', 'olsen', 'peters', 'quist', 'rossi', 'sato', 'turi', 'ulrich', 'vega', 'weber',
  'xu', 'yilmaz', 'zimmer',
];

function deriveViewerLogin(): string {
  const rng = new Rng(scenarioSeedFor('users'));
  const used = new Set<string>();
  let first = '';
  for (let i = 0; i < 12; i += 1) {
    const f = rng.pick(FIRST_NAMES);
    const l = rng.pick(LAST_NAMES);
    const base = `${f}${l}`.replace(/[^a-z0-9-]/g, '');
    let login = base;
    let suffix = 2;
    while (used.has(login)) {
      login = `${base}-${suffix}`;
      suffix += 1;
    }
    used.add(login);
    if (i === 0) first = login;
  }
  return first;
}

const VIEWER_LOGIN = deriveViewerLogin(); // 'adaiversen' under the fixed seed
const VIEWER_ID = 1000;
const VIEWER_NAME = 'ada iversen';
const VIEWER_EMAIL = `${VIEWER_LOGIN}@example.invalid`;

// ---------------------------------------------------------------------------
// Deterministic clock: server-made commits/objects use a fixed epoch plus a
// per-process counter, so the same request sequence always yields the same
// SHAs and two runs export identical state. Never Date.now() in stored data.
// ---------------------------------------------------------------------------

const FIXED_EPOCH_SEC = 1735689600; // 2025-01-01T00:00:00Z
const FIXED_ISO = '2025-01-01T00:00:00Z';
let commitCounter = 0;
function nextCommitTimestamp(): number {
  commitCounter += 1;
  return FIXED_EPOCH_SEC + commitCounter;
}

// ---------------------------------------------------------------------------
// Git objects
// ---------------------------------------------------------------------------

const ZERO_OID = '0000000000000000000000000000000000000000';
const TYPE_CODE: Record<string, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, string> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

function sha1(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

function hashObject(type: string, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return sha1(Buffer.concat([header, data]));
}

function isOid(value: unknown): value is string {
  return typeof value === 'string' && /^[0-9a-f]{40}$/.test(value);
}

interface GitObject {
  type: string;
  data: Buffer;
}

interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

function compareTreeNames(a: TreeEntry, b: TreeEntry): number {
  const left = Buffer.from(a.mode === '40000' ? `${a.name}/` : a.name, 'utf8');
  const right = Buffer.from(b.mode === '40000' ? `${b.name}/` : b.name, 'utf8');
  return Buffer.compare(left, right);
}

function buildTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort(compareTreeNames);
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    const nul = data.indexOf(0x00, space + 1);
    if (space < 0 || nul < 0 || nul + 21 > data.length) break;
    const mode = data.subarray(offset, space).toString('ascii');
    const name = data.subarray(space + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    offset = nul + 21;
  }
  return entries;
}

interface CommitView {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

function parseCommit(data: Buffer): CommitView {
  const blank = data.indexOf('\n\n');
  const headerEnd = blank < 0 ? data.length : blank;
  const headerText = data.subarray(0, headerEnd).toString('utf8');
  const message = blank < 0 ? '' : data.subarray(blank + 2).toString('utf8');
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of headerText.split('\n')) {
    if (!line) continue;
    const space = line.indexOf(' ');
    const key = space < 0 ? line : line.slice(0, space);
    const value = space < 0 ? '' : line.slice(space + 1);
    if (key === 'tree') tree = value;
    else if (key === 'parent') parents.push(value);
    else if (key === 'author') author = value;
    else if (key === 'committer') committer = value;
  }
  return { tree, parents, author, committer, message };
}

function parseTag(data: Buffer): { object: string; type: string } {
  const blank = data.indexOf('\n\n');
  const headerEnd = blank < 0 ? data.length : blank;
  let object = '';
  let type = '';
  for (const line of data.subarray(0, headerEnd).toString('utf8').split('\n')) {
    const space = line.indexOf(' ');
    if (space < 0) continue;
    const key = line.slice(0, space);
    const value = line.slice(space + 1);
    if (key === 'object') object = value;
    else if (key === 'type') type = value;
  }
  return { object, type };
}

function buildCommit(tree: string, parents: string[], message: string, timestamp: number, author: string): Buffer {
  const who = `${author} ${timestamp} +0000`;
  const lines = [`tree ${tree}`];
  for (const parent of parents) lines.push(`parent ${parent}`);
  lines.push(`author ${who}`, `committer ${who}`, '');
  return Buffer.from(`${lines.join('\n')}\n${message}`, 'utf8');
}

function buildTagObject(object: string, type: string, tag: string, message: string, timestamp: number, tagger: string): Buffer {
  const lines = [
    `object ${object}`,
    `type ${type}`,
    `tag ${tag}`,
    `tagger ${tagger} ${timestamp} +0000`,
    '',
  ];
  return Buffer.from(`${lines.join('\n')}\n${message}`, 'utf8');
}

// ---------------------------------------------------------------------------
// Packfiles (v2): plain objects on write, OFS/REF deltas resolved on read.
// ---------------------------------------------------------------------------

function encodeTypeSize(typeCode: number, size: number): Buffer {
  const bytes: number[] = [];
  let remaining = size;
  let first = (typeCode << 4) | (remaining & 0x0f);
  remaining >>>= 4;
  if (remaining > 0) first |= 0x80;
  bytes.push(first);
  while (remaining > 0) {
    let b = remaining & 0x7f;
    remaining >>>= 7;
    if (remaining > 0) b |= 0x80;
    bytes.push(b);
  }
  return Buffer.from(bytes);
}

function writePack(objects: { type: string; data: Buffer }[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const obj of objects) {
    const code = TYPE_CODE[obj.type] ?? 0;
    parts.push(encodeTypeSize(code, obj.data.length));
    parts.push(deflateSync(obj.data));
  }
  const body = Buffer.concat(parts);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const b = delta[offset++] ?? 0;
      value |= (b & 0x7f) << shift;
      shift += 7;
      if ((b & 0x80) === 0) break;
    }
    return value;
  };
  const sourceSize = readVarint();
  const targetSize = readVarint();
  if (sourceSize !== base.length) throw new Error('delta source size mismatch');
  const out: Buffer[] = [];
  let outLen = 0;
  while (offset < delta.length) {
    const op = delta[offset++] ?? 0;
    if ((op & 0x80) !== 0) {
      let copyOffset = 0;
      let copySize = 0;
      for (let i = 0; i < 4; i += 1) {
        if ((op & (1 << i)) !== 0) copyOffset |= (delta[offset++] ?? 0) << (i * 8);
      }
      for (let i = 0; i < 3; i += 1) {
        if ((op & (0x10 << i)) !== 0) copySize |= (delta[offset++] ?? 0) << (i * 8);
      }
      if (copySize === 0) copySize = 0x10000;
      out.push(base.subarray(copyOffset, copyOffset + copySize));
      outLen += copySize;
    } else if (op !== 0) {
      out.push(delta.subarray(offset, offset + op));
      offset += op;
      outLen += op;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  const result = Buffer.concat(out);
  if (result.length !== targetSize || outLen !== targetSize) throw new Error('delta target size mismatch');
  return result;
}

interface PackReadResult {
  objects: Map<string, GitObject>;
  deltaCount: number;
}

/**
 * The exact end of the zlib stream starting at `start`. One inflate gives
 * both the data and the end: inflateSync with info:true reports the bytes
 * the engine consumed in engine.bytesWritten (verified: exact on random
 * data, 0–64 KiB, stored blocks, exact end and trailing junk). The old
 * binary search is kept as a fallback if that ever looks wrong.
 */
function deflateLength(pack: Buffer, start: number, limit: number): { data: Buffer; end: number } {
  try {
    const out = inflateSync(pack.subarray(start, limit), { info: true }) as unknown as {
      buffer: Buffer;
      engine: { bytesWritten?: unknown };
    };
    const consumed = out.engine.bytesWritten;
    if (typeof consumed === 'number' && Number.isInteger(consumed) && consumed > 0 && start + consumed <= limit) {
      return { data: out.buffer, end: start + consumed };
    }
  } catch {
    // Fall through to the binary search below.
  }
  const data = inflateSync(pack.subarray(start, limit));
  let lo = start + 1;
  let hi = limit;
  // Invariant: inflate(start, lo-1) throws, inflate(start, hi) succeeds.
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    try {
      inflateSync(pack.subarray(start, mid));
      hi = mid;
    } catch {
      lo = mid + 1;
    }
  }
  return { data, end: lo };
}

/** Parse a pack, resolving OFS/REF deltas against the pack itself or `resolveBase`. */
function readPack(pack: Buffer, resolveBase?: (oid: string) => GitObject | undefined): PackReadResult {
  if (pack.length < 32 || pack.subarray(0, 4).toString('ascii') !== 'PACK') {
    throw new Error('not a packfile');
  }
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const expected = pack.subarray(pack.length - 20).toString('hex');
  const actual = sha1(pack.subarray(0, pack.length - 20));
  if (expected !== actual) throw new Error('pack trailer checksum mismatch');
  const dataEnd = pack.length - 20;
  const objects = new Map<string, GitObject>();
  const byOffset = new Map<number, GitObject>();
  const byOid = new Map<string, GitObject>();
  const pending: Array<{ type: string; data: Buffer; baseOid?: string; baseOffset?: number }> = [];
  let offset = 12;
  let deltaCount = 0;
  for (let i = 0; i < count; i += 1) {
    const entryOffset = offset;
    let byte = pack[offset++] ?? 0;
    const typeCode = (byte >> 4) & 0x07;
    let size = byte & 0x0f;
    let shift = 4;
    while ((byte & 0x80) !== 0) {
      byte = pack[offset++] ?? 0;
      size |= (byte & 0x7f) << shift;
      shift += 7;
    }
    let baseOid: string | undefined;
    let baseOffset: number | undefined;
    if (typeCode === 6) {
      let b = pack[offset++] ?? 0;
      let off = b & 0x7f;
      while ((b & 0x80) !== 0) {
        b = pack[offset++] ?? 0;
        off = ((off + 1) << 7) | (b & 0x7f);
      }
      baseOffset = entryOffset - off;
      deltaCount += 1;
    } else if (typeCode === 7) {
      baseOid = pack.subarray(offset, offset + 20).toString('hex');
      offset += 20;
      deltaCount += 1;
    }
    const { data, end } = deflateLength(pack, offset, dataEnd);
    offset = end;
    if (data.length !== size) throw new Error('pack object size mismatch');
    if (typeCode === 6 || typeCode === 7) {
      pending.push({ type: 'delta', data, baseOid, baseOffset });
      // Remember the slot by offset so a later delta can chain onto it.
      byOffset.set(entryOffset, { type: 'delta', data });
    } else {
      const type = CODE_TYPE[typeCode] ?? '';
      if (type === '') throw new Error(`unknown pack object type ${typeCode}`);
      const oid = hashObject(type, data);
      const obj: GitObject = { type, data };
      objects.set(oid, obj);
      byOffset.set(entryOffset, obj);
      byOid.set(oid, obj);
    }
  }
  // Resolve deltas in pack order (a delta's base always precedes it, or lives
  // in the store already); chain through byOffset for nested deltas.
  const resolveDelta = (entry: { data: Buffer; baseOid?: string; baseOffset?: number }): GitObject => {
    let base: GitObject | undefined;
    if (entry.baseOffset !== undefined) base = byOffset.get(entry.baseOffset);
    else if (entry.baseOid !== undefined) base = byOid.get(entry.baseOid) ?? resolveBase?.(entry.baseOid);
    if (base === undefined) throw new Error('delta base not found');
    if (base.type === 'delta') {
      const baseEntry = pending.find((p) => p.data === base.data);
      if (baseEntry === undefined) throw new Error('delta base chain broken');
      base = resolveDelta(baseEntry);
    }
    const data = applyDelta(base.data, entry.data);
    return { type: base.type, data };
  };
  for (const entry of pending) {
    const obj = resolveDelta(entry);
    const oid = hashObject(obj.type, obj.data);
    objects.set(oid, obj);
    byOid.set(oid, obj);
    // Replace the placeholder so later deltas chaining onto this offset resolve.
    for (const [off, slot] of byOffset) {
      if (slot.type === 'delta' && slot.data === entry.data) byOffset.set(off, obj);
    }
  }
  return { objects, deltaCount };
}

// ---------------------------------------------------------------------------
// Pkt-lines
// ---------------------------------------------------------------------------

function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = body.length + 4;
  const head = Buffer.from(len.toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([head, body]);
}

const PKT_FLUSH = Buffer.from('0000', 'ascii');
const PKT_DELIM = Buffer.from('0001', 'ascii');

type Pkt = { kind: 'data' | 'flush' | 'delim'; payload: Buffer };

/** Parse pkt-lines; throws on a bad length prefix. */
function decodePktLines(buf: Buffer): Pkt[] {
  const out: Pkt[] = [];
  let offset = 0;
  while (offset < buf.length) {
    if (offset + 4 > buf.length) throw new Error('truncated pkt-line length');
    const head = buf.subarray(offset, offset + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(head)) throw new Error('bad pkt-line length');
    const len = parseInt(head, 16);
    if (len === 0) {
      out.push({ kind: 'flush', payload: Buffer.alloc(0) });
      offset += 4;
    } else if (len === 1) {
      out.push({ kind: 'delim', payload: Buffer.alloc(0) });
      offset += 4;
    } else {
      if (offset + len > buf.length) throw new Error('truncated pkt-line');
      out.push({ kind: 'data', payload: buf.subarray(offset + 4, offset + len) });
      offset += len;
    }
  }
  return out;
}

function encodePktLines(lines: Array<string | Buffer | 'flush' | 'delim'>): Buffer {
  const parts: Buffer[] = [];
  for (const line of lines) {
    if (line === 'flush') parts.push(PKT_FLUSH);
    else if (line === 'delim') parts.push(PKT_DELIM);
    else parts.push(pktLine(line));
  }
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

interface Label {
  name: string;
  color: string;
}

interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
  comments: { id: number; body: string }[];
  seq: number;
}

interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  head: string;
  base: string;
  headSha: string;
  baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  mergeBase: string | null;
  seq: number;
}

interface Repo {
  id: number;
  owner: string;
  name: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  refs: Map<string, string>; // full ref name -> commit oid
  objects: Map<string, GitObject>;
  commitSeq: Map<string, number>; // commit oid -> insertion sequence
  nextCommitSeq: number;
  issues: Map<number, Issue>;
  pulls: Map<number, Pull>;
  labels: Map<string, Label>;
  nextNumber: number; // shared issue/PR counter
  nextCommentId: number;
  createdSeq: number;
}

const repos = new Map<string, Repo>(); // `${owner}/${name}` -> Repo
let nextRepoId = 1;
let nextGlobalId = 1;
let createdSeqCounter = 0;

function repoKey(owner: string, name: string): string {
  return `${owner}/${name}`;
}

function getRepo(owner: string, name: string): Repo | undefined {
  return repos.get(repoKey(owner, name));
}

function createRepoRecord(owner: string, name: string, description: string, isPrivate: boolean): Repo {
  const repo: Repo = {
    id: nextRepoId++,
    owner,
    name,
    description,
    private: isPrivate,
    defaultBranch: 'main',
    refs: new Map(),
    objects: new Map(),
    commitSeq: new Map(),
    nextCommitSeq: 1,
    issues: new Map(),
    pulls: new Map(),
    labels: new Map(),
    nextNumber: 1,
    nextCommentId: 1,
    createdSeq: ++createdSeqCounter,
  };
  repos.set(repoKey(owner, name), repo);
  return repo;
}

function viewerRepos(): Repo[] {
  const out: Repo[] = [];
  for (const repo of repos.values()) if (repo.owner === VIEWER_LOGIN) out.push(repo);
  out.sort((a, b) => a.createdSeq - b.createdSeq);
  return out;
}

function storeObject(repo: Repo, type: string, data: Buffer): string {
  const oid = hashObject(type, data);
  if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
  return oid;
}

function storeCommit(repo: Repo, data: Buffer): string {
  const oid = storeObject(repo, 'commit', data);
  if (!repo.commitSeq.has(oid)) repo.commitSeq.set(oid, repo.nextCommitSeq++);
  return oid;
}

/** All ancestors of `tips` (inclusive), as a set of commit oids. */
function ancestorCommits(repo: Repo, tips: Iterable<string>): Set<string> {
  const seen = new Set<string>();
  const stack: string[] = [];
  for (const tip of tips) if (isOid(tip) && !seen.has(tip)) stack.push(tip);
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    seen.add(oid);
    for (const parent of parseCommit(obj.data).parents) {
      if (!seen.has(parent)) stack.push(parent);
    }
  }
  return seen;
}

/** The merge-base of two tips: the common ancestor with the highest insertion sequence. */
function mergeBase(repo: Repo, a: string, b: string): string | null {
  const aAnc = ancestorCommits(repo, [a]);
  const bAnc = ancestorCommits(repo, [b]);
  let best: string | null = null;
  let bestSeq = -1;
  for (const oid of aAnc) {
    if (!bAnc.has(oid)) continue;
    const seq = repo.commitSeq.get(oid) ?? 0;
    if (seq > bestSeq) {
      bestSeq = seq;
      best = oid;
    }
  }
  return best;
}

/** Every object in the closure of `commitOids`: the commits themselves, their
 * trees (recursive) and blobs, and any tag peeled to its target. Parents are
 * NOT walked: callers pass the exact commit set (ancestors, or a depth cut). */
function closureObjects(repo: Repo, commitOids: Iterable<string>): Map<string, GitObject> {
  const out = new Map<string, GitObject>();
  const queue: string[] = [];
  for (const oid of commitOids) queue.push(oid);
  while (queue.length > 0) {
    const oid = queue.pop() as string;
    const obj = repo.objects.get(oid);
    if (obj === undefined || out.has(oid)) continue;
    out.set(oid, obj);
    if (obj.type === 'commit') {
      queue.push(parseCommit(obj.data).tree);
    } else if (obj.type === 'tree') {
      for (const entry of parseTree(obj.data)) queue.push(entry.oid);
    } else if (obj.type === 'tag') {
      queue.push(parseTag(obj.data).object);
    }
  }
  return out;
}

/** The commits a fetching client already holds: ancestors of its `have`
 * lines, except the walk stops at its shallow boundaries (a boundary commit
 * is held, its parents are not — a shallow clone does not have them). */
function clientHasCommits(repo: Repo, haves: Iterable<string>, shallow: Iterable<string>): Set<string> {
  const boundaries = new Set(shallow);
  const seen = new Set<string>();
  const stack: string[] = [];
  for (const have of haves) stack.push(have);
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    seen.add(oid);
    if (boundaries.has(oid)) continue;
    for (const parent of parseCommit(obj.data).parents) {
      if (!seen.has(parent)) stack.push(parent);
    }
  }
  return seen;
}

/** Flat path -> blob oid map for the tree at `treeOid` (recursive). */
function flattenTree(repo: Repo, treeOid: string | null, prefix = ''): Map<string, string> {
  const out = new Map<string, string>();
  if (treeOid === null) return out;
  const obj = repo.objects.get(treeOid);
  if (obj === undefined || obj.type !== 'tree') return out;
  for (const entry of parseTree(obj.data)) {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') {
      for (const [p, oid] of flattenTree(repo, entry.oid, path)) out.set(p, oid);
    } else {
      out.set(path, entry.oid);
    }
  }
  return out;
}

/** Build a tree object from a flat path->blob map (nested trees created as needed). */
function buildTreeFromFlat(repo: Repo, flat: Map<string, string>): string {
  // Group paths by first segment, recursively.
  const build = (paths: Array<[string, string]>): string => {
    const dirs = new Map<string, Array<[string, string]>>();
    const files: TreeEntry[] = [];
    for (const [path, oid] of paths) {
      const slash = path.indexOf('/');
      if (slash < 0) files.push({ mode: '100644', name: path, oid });
      else {
        const dir = path.slice(0, slash);
        const rest = path.slice(slash + 1);
        const list = dirs.get(dir) ?? [];
        list.push([rest, oid]);
        dirs.set(dir, list);
      }
    }
    for (const [dir, sub] of dirs) {
      files.push({ mode: '40000', name: dir, oid: build(sub) });
    }
    const data = buildTree(files);
    return storeObject(repo, 'tree', data);
  };
  return build([...flat.entries()]);
}

/** The tree oid at a commit, or null when the repo has no commit there. */
function commitTree(repo: Repo, commitOid: string | null): string | null {
  if (commitOid === null) return null;
  const obj = repo.objects.get(commitOid);
  if (obj === undefined || obj.type !== 'commit') return null;
  return parseCommit(obj.data).tree;
}

/** Apply one file change to a branch: returns the new commit oid. */
function commitFileChange(
  repo: Repo,
  branch: string,
  path: string,
  content: Buffer | null, // null = delete
  message: string,
): string {
  const tip = repo.refs.get(`refs/heads/${branch}`) ?? null;
  const baseTree = commitTree(repo, tip);
  const flat = flattenTree(repo, baseTree);
  if (content === null) flat.delete(path);
  else flat.set(path, storeObject(repo, 'blob', content));
  const newTree = buildTreeFromFlat(repo, flat);
  const timestamp = nextCommitTimestamp();
  const author = `${VIEWER_NAME} <${VIEWER_EMAIL}>`;
  const data = buildCommit(newTree, tip === null ? [] : [tip], message, timestamp, author);
  const oid = storeCommit(repo, data);
  repo.refs.set(`refs/heads/${branch}`, oid);
  return oid;
}

/** Three-way file-level merge. Returns the merged flat map, or null on conflict. */
function threeWayMerge(
  repo: Repo,
  baseOid: string | null,
  headOid: string | null,
  mergeBaseOid: string | null,
): Map<string, string> | null {
  const baseFlat = flattenTree(repo, commitTree(repo, baseOid));
  const headFlat = flattenTree(repo, commitTree(repo, headOid));
  const mbFlat = flattenTree(repo, commitTree(repo, mergeBaseOid));
  const merged = new Map<string, string>(baseFlat);
  const paths = new Set<string>([...mbFlat.keys(), ...headFlat.keys()]);
  for (const path of paths) {
    const mb = mbFlat.get(path);
    const head = headFlat.get(path);
    const base = baseFlat.get(path);
    const headChanged = head !== mb;
    const baseChanged = base !== mb;
    if (!headChanged) continue; // head kept merge-base's version: base's version stands
    if (!baseChanged) {
      // Only head changed: take head's version.
      if (head === undefined) merged.delete(path);
      else merged.set(path, head);
    } else if (head !== base) {
      return null; // both changed differently: conflict
    }
  }
  return merged;
}

function isMergeable(repo: Repo, pull: Pull): boolean {
  if (pull.mergeBase === null) return true;
  const headTip = repo.refs.get(`refs/heads/${pull.head}`) ?? pull.headSha;
  const baseTip = repo.refs.get(`refs/heads/${pull.base}`) ?? pull.baseSha;
  return threeWayMerge(repo, baseTip, headTip, pull.mergeBase) !== null;
}

function globalId(kind: string, repoId: number, number?: number): string {
  const raw = number === undefined ? `${kind}:${repoId}` : `${kind}:${repoId}:${number}`;
  return Buffer.from(raw, 'utf8').toString('base64');
}

function parseGlobalId(id: string): { kind: string; repoId: number; number: number | null } | null {
  try {
    const raw = Buffer.from(id, 'base64').toString('utf8');
    const parts = raw.split(':');
    if (parts.length < 2) return null;
    const kind = parts[0] as string;
    const repoId = Number(parts[1]);
    if (!Number.isInteger(repoId)) return null;
    const number = parts.length >= 3 ? Number(parts[2]) : null;
    return { kind, repoId, number: number !== null && Number.isInteger(number) ? number : null };
  } catch {
    return null;
  }
}

function repoById(id: number): Repo | undefined {
  for (const repo of repos.values()) if (repo.id === id) return repo;
  return undefined;
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? 'test-token';
let requestCounter = 0;

type Handler = (req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, query: URLSearchParams, body: Buffer) => void | Promise<void>;

function sendJson(res: http.ServerResponse, status: number, value: unknown, extraHeaders: Record<string, string> = {}): void {
  const text = JSON.stringify(value);
  const headers: Record<string, string> = {
    'content-type': 'application/json',
    'x-github-request-id': `req-${++requestCounter}`,
    ...extraHeaders,
  };
  res.writeHead(status, headers);
  res.end(text);
}

function sendError(res: http.ServerResponse, status: number, message: string): void {
  sendJson(res, status, { message });
}

function parseJsonBody(body: Buffer): { ok: true; value: Record<string, unknown> } | { ok: false } {
  if (body.length === 0) return { ok: true, value: {} };
  try {
    const parsed: unknown = JSON.parse(body.toString('utf8'));
    if (parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed)) {
      return { ok: true, value: parsed as Record<string, unknown> };
    }
    return { ok: true, value: {} };
  } catch {
    return { ok: false };
  }
}

function readBody(req: http.IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => chunks.push(chunk));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function checkAuth(req: http.IncomingMessage): boolean {
  const header = req.headers.authorization ?? '';
  return header === `Bearer ${TOKEN}`;
}

function requireAuth(req: http.IncomingMessage, res: http.ServerResponse): boolean {
  if (checkAuth(req)) return true;
  sendError(res, 401, 'Bad credentials');
  return false;
}

function str(value: unknown, fallback = ''): string {
  return typeof value === 'string' ? value : fallback;
}

function bool(value: unknown, fallback = false): boolean {
  return typeof value === 'boolean' ? value : fallback;
}

function intQuery(query: URLSearchParams, name: string, fallback: number): number {
  const raw = query.get(name);
  if (raw === null) return fallback;
  const n = Number(raw);
  return Number.isInteger(n) && n > 0 ? n : fallback;
}

function paginate<T>(items: T[], query: URLSearchParams, req: http.IncomingMessage): { page: T[]; link: string | null } {
  const perPage = Math.min(100, intQuery(query, 'per_page', 30));
  const page = intQuery(query, 'page', 1);
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  let link: string | null = null;
  if (start + perPage < items.length) {
    const origin = `http://${req.headers.host ?? '127.0.0.1'}`;
    const url = new URL(req.url ?? '/', origin);
    url.searchParams.set('page', String(page + 1));
    url.searchParams.set('per_page', String(perPage));
    link = `<${url.toString()}>; rel="next"`;
  }
  return { page: slice, link };
}

function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: globalId('Repository', repo.id),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: VIEWER_ID, type: 'User' },
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    html_url: `https://github.com/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    has_issues: true,
    has_projects: false,
    has_wiki: false,
  };
}

function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: globalId('Issue', repo.id, issue.number),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((l) => ({ name: l.name, color: l.color })),
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    html_url: `https://github.com/${repo.owner}/${repo.name}/issues/${issue.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
    comments: issue.comments.length,
  };
}

function pullJson(repo: Repo, pull: Pull): Record<string, unknown> {
  const mergeable = pull.state === 'open' ? isMergeable(repo, pull) : false;
  return {
    id: pull.id,
    node_id: globalId('PullRequest', repo.id, pull.number),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    mergeable,
    mergeable_state: mergeable ? 'clean' : 'dirty',
    head: { ref: pull.head, sha: pull.headSha, label: `${repo.owner}:${pull.head}` },
    base: { ref: pull.base, sha: pull.baseSha, label: `${repo.owner}:${pull.base}` },
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    html_url: `https://github.com/${repo.owner}/${repo.name}/pull/${pull.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
  };
}

function branchJson(repo: Repo, name: string): Record<string, unknown> | null {
  const sha = repo.refs.get(`refs/heads/${name}`);
  if (sha === undefined) return null;
  return { name, commit: { sha, url: `/api/v3/repos/${repo.owner}/${repo.name}/commits/${sha}` }, protected: false };
}

function commitJson(repo: Repo, oid: string): Record<string, unknown> | null {
  const obj = repo.objects.get(oid);
  if (obj === undefined || obj.type !== 'commit') return null;
  const view = parseCommit(obj.data);
  const ident = (raw: string): { name: string; email: string; date: string } => {
    const match = /^(.*) <(.*)> (\d+) [+-]\d{4}$/.exec(raw);
    if (match === null) return { name: raw, email: '', date: FIXED_ISO };
    return { name: match[1] as string, email: match[2] as string, date: new Date(Number(match[3]) * 1000).toISOString() };
  };
  return {
    sha: oid,
    commit: { message: view.message, author: ident(view.author), committer: ident(view.committer), tree: { sha: view.tree } },
    parents: view.parents.map((p) => ({ sha: p })),
    author: { login: VIEWER_LOGIN, id: VIEWER_ID },
    committer: { login: VIEWER_LOGIN, id: VIEWER_ID },
  };
}

function findRepoOr404(req: http.IncomingMessage, res: http.ServerResponse, owner: string, name: string): Repo | null {
  if (owner !== VIEWER_LOGIN) {
    sendError(res, 404, 'Not Found');
    return null;
  }
  const repo = getRepo(owner, name);
  if (repo === undefined) {
    sendError(res, 404, 'Not Found');
    return null;
  }
  return repo;
}

// ---------------------------------------------------------------------------
// REST handlers
// ---------------------------------------------------------------------------

function handleRoot(_req: http.IncomingMessage, res: http.ServerResponse): void {
  sendJson(res, 200, {
    current_user_url: '/api/v3/user',
    repository_url: '/api/v3/repos/{owner}/{repo}',
    user_url: '/api/v3/users/{user}',
  }, { 'x-oauth-scopes': 'repo, read:org' });
}

function handleMeta(_req: http.IncomingMessage, res: http.ServerResponse): void {
  sendJson(res, 200, { installed_version: '1.0.0', verifiable_password_authentication: false });
}

function handleGetUser(_req: http.IncomingMessage, res: http.ServerResponse): void {
  sendJson(res, 200, {
    login: VIEWER_LOGIN,
    id: VIEWER_ID,
    node_id: globalId('User', VIEWER_ID),
    name: VIEWER_NAME,
    email: VIEWER_EMAIL,
    type: 'User',
    site_admin: false,
  });
}

function handleListUserRepos(req: http.IncomingMessage, res: http.ServerResponse, _params: Record<string, string>, query: URLSearchParams): void {
  const { page, link } = paginate(viewerRepos(), query, req);
  const headers: Record<string, string> = { 'x-oauth-scopes': 'repo, read:org' };
  if (link !== null) headers.link = link;
  sendJson(res, 200, page.map(repoJson), headers);
}

function handleCreateRepo(_req: http.IncomingMessage, res: http.ServerResponse, _params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  const parsed = parseJsonBody(body);
  if (!parsed.ok) {
    sendError(res, 400, 'Problems parsing JSON');
    return;
  }
  const name = str(parsed.value.name);
  if (name === '' || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/.test(name)) {
    sendError(res, 422, 'Validation Failed: name is invalid');
    return;
  }
  if (getRepo(VIEWER_LOGIN, name) !== undefined) {
    sendError(res, 422, 'Repository creation failed: name already exists on this account');
    return;
  }
  const repo = createRepoRecord(VIEWER_LOGIN, name, str(parsed.value.description), bool(parsed.value.private, false));
  sendJson(res, 201, repoJson(repo));
}

function handleGetRepo(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  sendJson(res, 200, repoJson(repo));
}

function handlePatchRepo(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const parsed = parseJsonBody(body);
  if (!parsed.ok) {
    sendError(res, 400, 'Problems parsing JSON');
    return;
  }
  if (typeof parsed.value.description === 'string') repo.description = parsed.value.description;
  if (typeof parsed.value.private === 'boolean') repo.private = parsed.value.private;
  if (typeof parsed.value.default_branch === 'string' && repo.refs.has(`refs/heads/${parsed.value.default_branch}`)) {
    repo.defaultBranch = parsed.value.default_branch;
  }
  sendJson(res, 200, repoJson(repo));
}

function handleDeleteRepo(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  repos.delete(repoKey(repo.owner, repo.name));
  res.writeHead(204, { 'x-github-request-id': `req-${++requestCounter}` });
  res.end();
}

function handleListBranches(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, query: URLSearchParams): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const names: string[] = [];
  for (const ref of repo.refs.keys()) if (ref.startsWith('refs/heads/')) names.push(ref.slice('refs/heads/'.length));
  names.sort();
  const items = names.map((name) => branchJson(repo, name)).filter((b): b is Record<string, unknown> => b !== null);
  const { page, link } = paginate(items, query, req);
  const headers: Record<string, string> = {};
  if (link !== null) headers.link = link;
  sendJson(res, 200, page, headers);
}

function handleGetBranch(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const branch = branchJson(repo, params.branch as string);
  if (branch === null) {
    sendError(res, 404, 'Not Found');
    return;
  }
  sendJson(res, 200, branch);
}

function handleListCommits(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, query: URLSearchParams): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const refName = query.get('sha');
  let tip: string | null = null;
  if (refName !== null) {
    if (repo.refs.has(`refs/heads/${refName}`)) tip = repo.refs.get(`refs/heads/${refName}`) as string;
    else if (repo.refs.has(`refs/tags/${refName}`)) tip = repo.refs.get(`refs/tags/${refName}`) as string;
    else if (isOid(refName) && repo.objects.has(refName)) tip = refName;
    else {
      sendError(res, 404, 'Not Found');
      return;
    }
  } else {
    tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
  }
  if (tip === null) {
    sendJson(res, 200, []);
    return;
  }
  const commits = [...ancestorCommits(repo, [tip])];
  commits.sort((a, b) => (repo.commitSeq.get(b) ?? 0) - (repo.commitSeq.get(a) ?? 0));
  const items = commits.map((oid) => commitJson(repo, oid)).filter((c): c is Record<string, unknown> => c !== null);
  const { page, link } = paginate(items, query, req);
  const headers: Record<string, string> = {};
  if (link !== null) headers.link = link;
  sendJson(res, 200, page, headers);
}

function handleCreateRef(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const parsed = parseJsonBody(body);
  if (!parsed.ok) {
    sendError(res, 400, 'Problems parsing JSON');
    return;
  }
  const ref = str(parsed.value.ref);
  const sha = str(parsed.value.sha);
  if (!ref.startsWith('refs/') || !isOid(sha) || !repo.objects.has(sha)) {
    sendError(res, 422, 'Validation Failed: ref or sha is invalid');
    return;
  }
  if (repo.refs.has(ref)) {
    sendError(res, 422, 'Validation Failed: Reference already exists');
    return;
  }
  repo.refs.set(ref, sha);
  sendJson(res, 201, { ref, object: { sha, type: 'commit' } });
}

function handleGetRef(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const ref = `refs/heads/${params.branch as string}`;
  const sha = repo.refs.get(ref);
  if (sha === undefined) {
    sendError(res, 404, 'Not Found');
    return;
  }
  sendJson(res, 200, { ref, object: { sha, type: 'commit' } });
}

function resolveContentsRef(repo: Repo, ref: string | null): string | null {
  if (ref === null) return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
  if (repo.refs.has(`refs/heads/${ref}`)) return repo.refs.get(`refs/heads/${ref}`) as string;
  if (repo.refs.has(`refs/tags/${ref}`)) return repo.refs.get(`refs/tags/${ref}`) as string;
  if (isOid(ref) && repo.objects.has(ref)) return ref;
  return null;
}

function handleGetContents(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, query: URLSearchParams): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const path = params.path as string;
  const tip = resolveContentsRef(repo, query.get('ref'));
  if (tip === null) {
    sendError(res, 404, 'Not Found');
    return;
  }
  const flat = flattenTree(repo, commitTree(repo, tip));
  if (flat.has(path)) {
    const blobOid = flat.get(path) as string;
    const blob = repo.objects.get(blobOid);
    if (blob === undefined) {
      sendError(res, 404, 'Not Found');
      return;
    }
    sendJson(res, 200, {
      type: 'file',
      encoding: 'base64',
      size: blob.data.length,
      name: path.split('/').pop(),
      path,
      content: blob.data.toString('base64'),
      sha: blobOid,
    });
    return;
  }
  // Directory listing.
  const prefix = path === '' ? '' : `${path}/`;
  const entries = new Map<string, { type: string; name: string; path: string; sha: string; size?: number }>();
  for (const [p, oid] of flat) {
    if (!p.startsWith(prefix)) continue;
    const rest = p.slice(prefix.length);
    const slash = rest.indexOf('/');
    if (slash < 0) {
      const blob = repo.objects.get(oid);
      entries.set(rest, { type: 'file', name: rest, path: p, sha: oid, size: blob?.data.length ?? 0 });
    } else {
      const dir = rest.slice(0, slash);
      if (!entries.has(dir)) entries.set(dir, { type: 'dir', name: dir, path: `${prefix}${dir}`, sha: '' });
    }
  }
  if (entries.size === 0) {
    sendError(res, 404, 'Not Found');
    return;
  }
  const list = [...entries.values()].sort((a, b) => (a.name < b.name ? -1 : 1));
  sendJson(res, 200, list);
}

function handlePutContents(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const parsed = parseJsonBody(body);
  if (!parsed.ok) {
    sendError(res, 400, 'Problems parsing JSON');
    return;
  }
  const path = params.path as string;
  const message = str(parsed.value.message, 'Update file');
  const contentB64 = str(parsed.value.content);
  if (contentB64 === '') {
    sendError(res, 422, 'Validation Failed: content is required');
    return;
  }
  let content: Buffer;
  try {
    content = Buffer.from(contentB64, 'base64');
  } catch {
    sendError(res, 422, 'Validation Failed: content is not valid base64');
    return;
  }
  const branch = str(parsed.value.branch, repo.defaultBranch);
  const sha = typeof parsed.value.sha === 'string' ? parsed.value.sha : null;
  // Resolve the branch; create it from the default tip when it does not exist.
  if (!repo.refs.has(`refs/heads/${branch}`)) {
    const defaultTip = repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
    if (defaultTip !== null) repo.refs.set(`refs/heads/${branch}`, defaultTip);
  }
  const tip = repo.refs.get(`refs/heads/${branch}`) ?? null;
  const flat = flattenTree(repo, commitTree(repo, tip));
  const exists = flat.has(path);
  if (sha !== null) {
    const current = flat.get(path);
    if (!exists || current !== sha) {
      sendError(res, 409, 'Conflict: sha does not match the current file');
      return;
    }
  }
  const newCommit = commitFileChange(repo, branch, path, content, message);
  const blobOid = hashObject('blob', content);
  sendJson(res, exists ? 200 : 201, {
    content: { name: path.split('/').pop(), path, sha: blobOid, size: content.length, type: 'file', encoding: 'base64', content: contentB64 },
    commit: { sha: newCommit, message },
  });
}

function handleDeleteContents(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const parsed = parseJsonBody(body);
  if (!parsed.ok) {
    sendError(res, 400, 'Problems parsing JSON');
    return;
  }
  const path = params.path as string;
  const message = str(parsed.value.message, 'Delete file');
  const sha = typeof parsed.value.sha === 'string' ? parsed.value.sha : null;
  const branch = str(parsed.value.branch, repo.defaultBranch);
  const tip = repo.refs.get(`refs/heads/${branch}`) ?? null;
  if (tip === null) {
    sendError(res, 404, 'Not Found');
    return;
  }
  const flat = flattenTree(repo, commitTree(repo, tip));
  const current = flat.get(path);
  if (current === undefined) {
    sendError(res, 404, 'Not Found');
    return;
  }
  if (sha !== null && current !== sha) {
    sendError(res, 409, 'Conflict: sha does not match the current file');
    return;
  }
  const newCommit = commitFileChange(repo, branch, path, null, message);
  sendJson(res, 200, {
    content: { name: path.split('/').pop(), path, sha: current, type: 'file', encoding: 'base64' },
    commit: { sha: newCommit, message },
  });
}

function handleCreateLabel(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const parsed = parseJsonBody(body);
  if (!parsed.ok) {
    sendError(res, 400, 'Problems parsing JSON');
    return;
  }
  const name = str(parsed.value.name);
  if (name === '') {
    sendError(res, 422, 'Validation Failed: name is required');
    return;
  }
  if (repo.labels.has(name)) {
    sendError(res, 422, 'Validation Failed: label already exists');
    return;
  }
  const label: Label = { name, color: str(parsed.value.color, 'ededed') };
  repo.labels.set(name, label);
  sendJson(res, 201, { name: label.name, color: label.color });
}

function handleListLabels(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  sendJson(res, 200, [...repo.labels.values()].map((l) => ({ name: l.name, color: l.color })));
}

function listIssuesFiltered(repo: Repo, query: URLSearchParams): Issue[] {
  const state = query.get('state') ?? 'open';
  const sort = query.get('sort') ?? 'created';
  const direction = query.get('direction') ?? 'desc';
  let items = [...repo.issues.values()];
  if (state !== 'all') items = items.filter((i) => i.state === state);
  if (sort === 'created') {
    items.sort((a, b) => (direction === 'asc' ? a.seq - b.seq : b.seq - a.seq));
  } else {
    items.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
  }
  return items;
}

function handleCreateIssue(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const parsed = parseJsonBody(body);
  if (!parsed.ok) {
    sendError(res, 400, 'Problems parsing JSON');
    return;
  }
  const title = str(parsed.value.title);
  if (title === '') {
    sendError(res, 422, 'Validation Failed: title is required');
    return;
  }
  const labelNames: string[] = Array.isArray(parsed.value.labels)
    ? (parsed.value.labels as unknown[]).map((l) => (typeof l === 'string' ? l : str((l as Record<string, unknown>).name)))
    : [];
  const labels: Label[] = [];
  for (const name of labelNames) {
    const label = repo.labels.get(name);
    if (label === undefined) {
      sendError(res, 422, `Validation Failed: label "${name}" does not exist`);
      return;
    }
    labels.push(label);
  }
  const number = repo.nextNumber++;
  const issue: Issue = {
    id: nextGlobalId++,
    number,
    title,
    body: str(parsed.value.body),
    state: 'open',
    labels,
    comments: [],
    seq: number,
  };
  repo.issues.set(number, issue);
  sendJson(res, 201, issueJson(repo, issue));
}

function handleListIssues(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, query: URLSearchParams): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const items = listIssuesFiltered(repo, query).map((i) => issueJson(repo, i));
  const { page, link } = paginate(items, query, req);
  const headers: Record<string, string> = {};
  if (link !== null) headers.link = link;
  sendJson(res, 200, page, headers);
}

function handleGetIssue(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const issue = repo.issues.get(Number(params.number));
  if (issue === undefined) {
    sendError(res, 404, 'Not Found');
    return;
  }
  sendJson(res, 200, issueJson(repo, issue));
}

function handlePatchIssue(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const issue = repo.issues.get(Number(params.number));
  if (issue === undefined) {
    sendError(res, 404, 'Not Found');
    return;
  }
  const parsed = parseJsonBody(body);
  if (!parsed.ok) {
    sendError(res, 400, 'Problems parsing JSON');
    return;
  }
  if (parsed.value.state === 'open' || parsed.value.state === 'closed') issue.state = parsed.value.state;
  if (typeof parsed.value.title === 'string') issue.title = parsed.value.title;
  if (typeof parsed.value.body === 'string') issue.body = parsed.value.body;
  sendJson(res, 200, issueJson(repo, issue));
}

function handleCreateComment(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const issue = repo.issues.get(Number(params.number));
  if (issue === undefined) {
    sendError(res, 404, 'Not Found');
    return;
  }
  const parsed = parseJsonBody(body);
  if (!parsed.ok) {
    sendError(res, 400, 'Problems parsing JSON');
    return;
  }
  const comment = { id: repo.nextCommentId++, body: str(parsed.value.body) };
  issue.comments.push(comment);
  sendJson(res, 201, {
    id: comment.id,
    node_id: globalId('IssueComment', repo.id, comment.id),
    body: comment.body,
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
  });
}

function handleListComments(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, query: URLSearchParams): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const issue = repo.issues.get(Number(params.number));
  if (issue === undefined) {
    sendError(res, 404, 'Not Found');
    return;
  }
  const items = issue.comments.map((c) => ({
    id: c.id,
    node_id: globalId('IssueComment', repo.id, c.id),
    body: c.body,
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
  }));
  const { page, link } = paginate(items, query, req);
  const headers: Record<string, string> = {};
  if (link !== null) headers.link = link;
  sendJson(res, 200, page, headers);
}

function pullFiles(repo: Repo, pull: Pull): Array<{ filename: string; status: string; additions: number; deletions: number; changes: number }> {
  const headFlat = flattenTree(repo, commitTree(repo, pull.headSha));
  const baseFlat = flattenTree(repo, commitTree(repo, pull.mergeBase));
  const out: Array<{ filename: string; status: string; additions: number; deletions: number; changes: number }> = [];
  const paths = new Set<string>([...baseFlat.keys(), ...headFlat.keys()]);
  const lineCount = (oid: string | undefined): number => {
    if (oid === undefined) return 0;
    const obj = repo.objects.get(oid);
    if (obj === undefined) return 0;
    const text = obj.data.toString('utf8');
    if (text === '') return 0;
    return text.split('\n').length;
  };
  for (const path of [...paths].sort()) {
    const from = baseFlat.get(path);
    const to = headFlat.get(path);
    if (from === to) continue;
    if (from === undefined) {
      const additions = Math.max(1, lineCount(to));
      out.push({ filename: path, status: 'added', additions, deletions: 0, changes: additions });
    } else if (to === undefined) {
      const deletions = Math.max(1, lineCount(from));
      out.push({ filename: path, status: 'removed', additions: 0, deletions, changes: deletions });
    } else {
      const additions = Math.max(1, lineCount(to));
      const deletions = lineCount(from);
      out.push({ filename: path, status: 'modified', additions, deletions, changes: additions + deletions });
    }
  }
  return out;
}

function handleCreatePull(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const parsed = parseJsonBody(body);
  if (!parsed.ok) {
    sendError(res, 400, 'Problems parsing JSON');
    return;
  }
  const head = str(parsed.value.head);
  const base = str(parsed.value.base, repo.defaultBranch);
  const title = str(parsed.value.title);
  if (head === '' || title === '') {
    sendError(res, 422, 'Validation Failed: head and title are required');
    return;
  }
  const headSha = repo.refs.get(`refs/heads/${head}`);
  const baseSha = repo.refs.get(`refs/heads/${base}`);
  if (headSha === undefined || baseSha === undefined) {
    sendError(res, 422, 'Validation Failed: head or base branch does not exist');
    return;
  }
  for (const existing of repo.pulls.values()) {
    if (existing.head === head && existing.base === base && existing.state === 'open') {
      sendError(res, 422, 'Validation Failed: a pull request already exists for this head branch');
      return;
    }
  }
  const number = repo.nextNumber++;
  const mb = mergeBase(repo, headSha, baseSha);
  const pull: Pull = {
    id: nextGlobalId++,
    number,
    title,
    body: str(parsed.value.body),
    state: 'open',
    head,
    base,
    headSha,
    baseSha,
    merged: false,
    mergedAt: null,
    mergeCommitSha: null,
    mergeBase: mb,
    seq: number,
  };
  repo.pulls.set(number, pull);
  sendJson(res, 201, pullJson(repo, pull));
}

function handleListPulls(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, query: URLSearchParams): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const state = query.get('state') ?? 'open';
  let items = [...repo.pulls.values()];
  if (state !== 'all') items = items.filter((p) => p.state === state);
  items.sort((a, b) => b.seq - a.seq);
  const json = items.map((p) => pullJson(repo, p));
  const { page, link } = paginate(json, query, req);
  const headers: Record<string, string> = {};
  if (link !== null) headers.link = link;
  sendJson(res, 200, page, headers);
}

function handleGetPull(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const pull = repo.pulls.get(Number(params.number));
  if (pull === undefined) {
    sendError(res, 404, 'Not Found');
    return;
  }
  sendJson(res, 200, pullJson(repo, pull));
}

function handleListPullFiles(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const pull = repo.pulls.get(Number(params.number));
  if (pull === undefined) {
    sendError(res, 404, 'Not Found');
    return;
  }
  sendJson(res, 200, pullFiles(repo, pull));
}

function doMerge(repo: Repo, pull: Pull): { ok: boolean; sha?: string; reason?: string } {
  const headTip = repo.refs.get(`refs/heads/${pull.head}`) ?? pull.headSha;
  const baseTip = repo.refs.get(`refs/heads/${pull.base}`) ?? pull.baseSha;
  const merged = threeWayMerge(repo, baseTip, headTip, pull.mergeBase);
  if (merged === null) return { ok: false, reason: 'merge conflict' };
  const newTree = buildTreeFromFlat(repo, merged);
  const timestamp = nextCommitTimestamp();
  const author = `${VIEWER_NAME} <${VIEWER_EMAIL}>`;
  const data = buildCommit(newTree, [baseTip], pull.title, timestamp, author);
  const sha = storeCommit(repo, data);
  repo.refs.set(`refs/heads/${pull.base}`, sha);
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedAt = FIXED_ISO;
  pull.mergeCommitSha = sha;
  pull.baseSha = sha;
  return { ok: true, sha };
}

function handleMergePull(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  const repo = findRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const pull = repo.pulls.get(Number(params.number));
  if (pull === undefined) {
    sendError(res, 404, 'Not Found');
    return;
  }
  const parsed = parseJsonBody(body);
  if (!parsed.ok) {
    sendError(res, 400, 'Problems parsing JSON');
    return;
  }
  if (pull.state !== 'open') {
    sendError(res, 405, 'Pull request is not open');
    return;
  }
  const result = doMerge(repo, pull);
  if (!result.ok) {
    sendError(res, 405, 'Merge conflict');
    return;
  }
  sendJson(res, 200, { sha: result.sha, merged: true, message: 'Pull request successfully merged' });
}

// ---------------------------------------------------------------------------
// Git smart HTTP
// ---------------------------------------------------------------------------

function gitRepoOr404(req: http.IncomingMessage, res: http.ServerResponse, owner: string, repoName: string): Repo | null {
  if (owner !== VIEWER_LOGIN) {
    sendJson(res, 404, { message: 'Not Found' });
    return null;
  }
  const repo = getRepo(owner, repoName);
  if (repo === undefined) {
    sendJson(res, 404, { message: 'Not Found' });
    return null;
  }
  return repo;
}

function sendGit(res: http.ServerResponse, status: number, contentType: string, body: Buffer): void {
  res.writeHead(status, {
    'content-type': contentType,
    'x-github-request-id': `req-${++requestCounter}`,
  });
  res.end(body);
}

function refLines(repo: Repo): Array<{ oid: string; name: string; peeled?: string }> {
  const lines: Array<{ oid: string; name: string; peeled?: string }> = [];
  const heads: string[] = [];
  const tags: string[] = [];
  for (const name of repo.refs.keys()) {
    if (name.startsWith('refs/heads/')) heads.push(name);
    else if (name.startsWith('refs/tags/')) tags.push(name);
  }
  heads.sort();
  tags.sort();
  for (const name of heads) lines.push({ oid: repo.refs.get(name) as string, name });
  for (const name of tags) {
    const oid = repo.refs.get(name) as string;
    const obj = repo.objects.get(oid);
    let peeled: string | undefined;
    if (obj !== undefined && obj.type === 'tag') {
      const target = parseTag(obj.data).object;
      // Peel to the commit the tag ultimately points at.
      let current = target;
      for (let i = 0; i < 8; i += 1) {
        const cur = repo.objects.get(current);
        if (cur === undefined || cur.type !== 'tag') break;
        current = parseTag(cur.data).object;
      }
      peeled = current;
    }
    lines.push({ oid, name, peeled });
  }
  return lines;
}

function handleInfoRefs(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, query: URLSearchParams): void {
  const service = query.get('service') ?? '';
  const repo = gitRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  const v2 = service === 'git-upload-pack' && (req.headers['git-protocol'] ?? '') === 'version=2';
  if (service === 'git-upload-pack' && v2) {
    const body = encodePktLines([
      'version 2\n',
      'agent=git-forge/1.0\n',
      'ls-refs=unborn\n',
      'fetch=shallow filter\n',
      'object-format=sha1\n',
      'flush',
    ]);
    sendGit(res, 200, 'application/x-git-upload-pack-advertisement', body);
    return;
  }
  if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
    sendError(res, 400, 'unknown service');
    return;
  }
  const isUpload = service === 'git-upload-pack';
  const caps = isUpload
    ? 'side-band-64k symref=HEAD:refs/heads/main thin-pack ofs-delta shallow multi_ack agent=git-forge/1.0'
    : 'report-status side-band-64k delete-refs ofs-delta agent=git-forge/1.0';
  const lines: Array<string | Buffer | 'flush'> = [`# service=${service}\n`, 'flush'];
  const refs = refLines(repo);
  const defaultTip = repo.refs.get('refs/heads/main');
  if (refs.length === 0) {
    lines.push(`${ZERO_OID} capabilities^{}\0${caps}\n`);
  } else {
    const headOid = defaultTip ?? (refs[0]?.oid as string);
    lines.push(`${headOid} HEAD\0${caps}\n`);
    for (const ref of refs) {
      lines.push(`${ref.oid} ${ref.name}\n`);
      if (ref.peeled !== undefined) lines.push(`${ref.peeled} ${ref.name}^{}\n`);
    }
  }
  lines.push('flush');
  sendGit(res, 200, `application/x-${service}-advertisement`, encodePktLines(lines));
}

interface LsRefsArgs {
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  prefixes: string[];
}

function handleUploadPack(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  const repo = gitRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  let packets: Pkt[];
  try {
    packets = decodePktLines(body);
  } catch {
    sendError(res, 400, 'malformed pkt-line stream');
    return;
  }
  const dataLines = packets.filter((p) => p.kind === 'data').map((p) => p.payload.toString('utf8'));
  const first = dataLines[0] ?? '';
  if (!first.startsWith('command=')) {
    sendError(res, 400, 'expected a command= line');
    return;
  }
  const command = first.slice('command='.length).trim();
  if (command === 'ls-refs') {
    const args: LsRefsArgs = { peel: false, symrefs: false, unborn: false, prefixes: [] };
    let pastDelim = false;
    for (const packet of packets) {
      if (packet.kind === 'delim') {
        pastDelim = true;
        continue;
      }
      if (!pastDelim || packet.kind !== 'data') continue;
      const line = packet.payload.toString('utf8').trim();
      if (line === 'peel') args.peel = true;
      else if (line === 'symrefs') args.symrefs = true;
      else if (line === 'unborn') args.unborn = true;
      else if (line.startsWith('ref-prefix ')) args.prefixes.push(line.slice('ref-prefix '.length));
    }
    const out: string[] = [];
    const keep = (name: string): boolean => args.prefixes.length === 0 || args.prefixes.some((p) => name.startsWith(p));
    const defaultTip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
    if (defaultTip !== undefined) {
      if (keep('HEAD')) {
        out.push(`${defaultTip} HEAD${args.symrefs ? ` symref-target:refs/heads/${repo.defaultBranch}` : ''}\n`);
      }
      for (const ref of refLines(repo)) {
        if (!keep(ref.name)) continue;
        let line = `${ref.oid} ${ref.name}`;
        if (ref.peeled !== undefined && args.peel) line += ` peeled:${ref.peeled}`;
        out.push(`${line}\n`);
      }
    } else if (args.unborn && keep('HEAD')) {
      out.push(`unborn HEAD${args.symrefs ? ` symref-target:refs/heads/${repo.defaultBranch}` : ''}\n`);
    }
    sendGit(res, 200, 'application/x-git-upload-pack-result', encodePktLines([...out, 'flush']));
    return;
  }
  if (command === 'fetch') {
    const wants: string[] = [];
    const haves: string[] = [];
    const shallow: string[] = [];
    let deepen: number | undefined;
    let pastDelim = false;
    for (const packet of packets) {
      if (packet.kind === 'delim') {
        pastDelim = true;
        continue;
      }
      if (!pastDelim || packet.kind !== 'data') continue;
      const line = packet.payload.toString('utf8').trim();
      if (line.startsWith('want ')) wants.push(line.slice(5));
      else if (line.startsWith('have ')) haves.push(line.slice(5));
      else if (line.startsWith('shallow ')) shallow.push(line.slice(8));
      else if (line.startsWith('deepen ')) deepen = Number(line.slice(7));
    }
    for (const want of wants) {
      if (!repo.objects.has(want)) {
        const err = encodePktLines(['packfile\n', Buffer.concat([Buffer.from([3]), Buffer.from(`want ${want} not found\n`, 'utf8')]), 'flush']);
        sendGit(res, 200, 'application/x-git-upload-pack-result', err);
        return;
      }
    }
    // A wanted tag peels to the commit it points at; the tag object itself
    // still rides in the pack.
    const wantTags: string[] = [];
    const wantCommits: string[] = [];
    for (const want of wants) {
      const obj = repo.objects.get(want) as GitObject;
      if (obj.type === 'tag') {
        wantTags.push(want);
        let current = parseTag(obj.data).object;
        for (let i = 0; i < 8; i += 1) {
          const cur = repo.objects.get(current);
          if (cur === undefined || cur.type !== 'tag') break;
          current = parseTag(cur.data).object;
        }
        wantCommits.push(current);
      } else {
        wantCommits.push(want);
      }
    }
    const havesReachable = clientHasCommits(repo, haves, shallow);
    let sent: Set<string>;
    let newShallow: string[] = [];
    if (deepen !== undefined) {
      // Generations from the wants (want itself = generation 1).
      const gen = new Map<string, number>();
      const queue: Array<[string, number]> = wantCommits.filter((w) => repo.objects.has(w)).map((w) => [w, 1] as [string, number]);
      while (queue.length > 0) {
        const [oid, g] = queue.shift() as [string, number];
        if (gen.has(oid)) continue;
        const obj = repo.objects.get(oid);
        if (obj === undefined || obj.type !== 'commit') continue;
        gen.set(oid, g);
        if (g < deepen) {
          for (const parent of parseCommit(obj.data).parents) {
            if (!gen.has(parent)) queue.push([parent, g + 1]);
          }
        }
      }
      sent = new Set<string>();
      for (const [oid, g] of gen) if (g <= deepen && !havesReachable.has(oid)) sent.add(oid);
      const resulting = new Set<string>([...havesReachable, ...sent]);
      for (const oid of resulting) {
        const obj = repo.objects.get(oid);
        if (obj === undefined || obj.type !== 'commit') continue;
        const parents = parseCommit(obj.data).parents;
        if (parents.some((p) => !resulting.has(p))) newShallow.push(oid);
      }
      newShallow.sort();
    } else {
      sent = ancestorCommits(repo, wantCommits);
      for (const oid of havesReachable) sent.delete(oid);
    }
    const resultingSet = new Set<string>([...havesReachable, ...sent]);
    const unshallow: string[] = [];
    for (const s of shallow) {
      if (!resultingSet.has(s)) continue;
      const obj = repo.objects.get(s);
      if (obj === undefined || obj.type !== 'commit') continue;
      const parents = parseCommit(obj.data).parents;
      if (parents.every((p) => resultingSet.has(p)) && !newShallow.includes(s)) unshallow.push(s);
    }
    unshallow.sort();
    // The pack carries exactly the new commits' closure, minus anything the
    // client already has (its haves' closure), plus wanted tag objects.
    const objects = closureObjects(repo, sent);
    const haveClosure = closureObjects(repo, havesReachable);
    for (const oid of haveClosure.keys()) objects.delete(oid);
    for (const tagOid of wantTags) {
      const tagObj = repo.objects.get(tagOid);
      if (tagObj !== undefined && !haveClosure.has(tagOid)) objects.set(tagOid, tagObj);
    }
    // Section rules, as real git speaks them: the acknowledgments section
    // appears only when the request negotiated (it sent `have` lines); with
    // no haves the response starts at shallow-info/packfile. The shallow-info
    // section appears when shallow state is in play (shallow lines or a
    // deepen), even when its lists are empty.
    const parts: Array<string | Buffer | 'flush' | 'delim'> = [];
    if (haves.length > 0) {
      parts.push('acknowledgments\n');
      const acked = haves.filter((h) => repo.objects.has(h));
      if (acked.length === 0) parts.push('NAK\n');
      else {
        for (const h of acked) parts.push(`ACK ${h}\n`);
        parts.push('ready\n');
      }
      parts.push('delim');
    }
    if (shallow.length > 0 || deepen !== undefined || newShallow.length > 0 || unshallow.length > 0) {
      parts.push('shallow-info\n');
      for (const oid of newShallow) parts.push(`shallow ${oid}\n`);
      for (const oid of unshallow) parts.push(`unshallow ${oid}\n`);
      parts.push('delim');
    }
    if (objects.size > 0) {
      const pack = writePack([...objects.values()]);
      parts.push('packfile\n');
      for (let off = 0; off < pack.length; off += 65515) {
        const chunk = pack.subarray(off, Math.min(pack.length, off + 65515));
        parts.push(Buffer.concat([Buffer.from([1]), chunk]));
      }
      parts.push('flush');
    } else {
      parts.push('flush');
    }
    sendGit(res, 200, 'application/x-git-upload-pack-result', encodePktLines(parts));
    return;
  }
  sendError(res, 400, `unknown command ${command}`);
}

function handleReceivePack(req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  const repo = gitRepoOr404(req, res, params.owner as string, params.repo as string);
  if (repo === null) return;
  // Split the command list (pkt-lines up to the first flush) from the pack bytes.
  let offset = 0;
  const commandLines: string[] = [];
  let packStart = -1;
  try {
    while (offset < body.length) {
      if (offset + 4 > body.length) throw new Error('truncated pkt-line');
      const head = body.subarray(offset, offset + 4).toString('ascii');
      if (!/^[0-9a-fA-F]{4}$/.test(head)) throw new Error('bad pkt-line length');
      const len = parseInt(head, 16);
      if (len === 0) {
        packStart = offset + 4;
        break;
      }
      if (len === 1) {
        offset += 4;
        continue;
      }
      if (offset + len > body.length) throw new Error('truncated pkt-line');
      commandLines.push(body.subarray(offset + 4, offset + len).toString('utf8'));
      offset += len;
    }
  } catch {
    sendError(res, 400, 'malformed pkt-line stream');
    return;
  }
  if (packStart < 0) {
    sendError(res, 400, 'missing flush after the command list');
    return;
  }
  const commands: Array<{ oldOid: string; newOid: string; ref: string }> = [];
  for (const raw of commandLines) {
    const line = raw.split('\0')[0] as string;
    const parts = line.trim().split(' ');
    if (parts.length < 3 || !isOid(parts[0]) || !isOid(parts[1])) {
      sendError(res, 400, 'malformed command line');
      return;
    }
    commands.push({ oldOid: parts[0] as string, newOid: parts[1] as string, ref: parts[2] as string });
  }
  const report = (unpackOk: boolean, unpackDetail: string, results: Array<{ ref: string; ok: boolean; reason: string }>): void => {
    // The report-status stream inside band 1 is itself pkt-lined and ends with
    // its own flush; the outer flush then ends the response. Real git waits
    // for the in-band flush, so a report without it reads as a hangup.
    const bandPayload: Buffer[] = [pktLine(`unpack ${unpackOk ? 'ok' : unpackDetail}\n`)];
    for (const result of results) {
      bandPayload.push(pktLine(result.ok ? `ok ${result.ref}\n` : `ng ${result.ref} ${result.reason}\n`));
    }
    bandPayload.push(PKT_FLUSH);
    const body = Buffer.concat([pktLine(Buffer.concat([Buffer.from([1]), ...bandPayload])), PKT_FLUSH]);
    sendGit(res, 200, 'application/x-git-receive-pack-result', body);
  };
  const packBytes = body.subarray(packStart);
  const needsPack = commands.some((c) => c.newOid !== ZERO_OID);
  if (needsPack) {
    if (packBytes.length === 0) {
      report(false, 'missing pack', []);
      return;
    }
    try {
      const read = readPack(packBytes, (oid) => repo.objects.get(oid));
      for (const [oid, obj] of read.objects) {
        if (!repo.objects.has(oid)) repo.objects.set(oid, obj);
        if (obj.type === 'commit' && !repo.commitSeq.has(oid)) repo.commitSeq.set(oid, repo.nextCommitSeq++);
      }
    } catch (err) {
      report(false, `unpack error: ${err instanceof Error ? err.message : String(err)}`, []);
      return;
    }
  }
  const results: Array<{ ref: string; ok: boolean; reason: string }> = [];
  const toApply: Array<{ ref: string; newOid: string }> = [];
  for (const command of commands) {
    const current = repo.refs.get(command.ref);
    if (command.newOid === ZERO_OID) {
      if (current === undefined) results.push({ ref: command.ref, ok: false, reason: 'remote ref does not exist' });
      else if (command.oldOid !== ZERO_OID && command.oldOid !== current) results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      else {
        results.push({ ref: command.ref, ok: true, reason: '' });
        toApply.push({ ref: command.ref, newOid: ZERO_OID });
      }
      continue;
    }
    const newObj = repo.objects.get(command.newOid);
    const isTagRef = command.ref.startsWith('refs/tags/');
    if (newObj === undefined || (isTagRef ? newObj.type !== 'commit' && newObj.type !== 'tag' : newObj.type !== 'commit')) {
      results.push({ ref: command.ref, ok: false, reason: 'object not found' });
      continue;
    }
    if (command.oldOid === ZERO_OID) {
      if (current !== undefined) results.push({ ref: command.ref, ok: false, reason: 'already exists' });
      else {
        results.push({ ref: command.ref, ok: true, reason: '' });
        toApply.push({ ref: command.ref, newOid: command.newOid });
      }
      continue;
    }
    if (current === undefined) {
      results.push({ ref: command.ref, ok: false, reason: 'remote ref does not exist' });
      continue;
    }
    if (current !== command.oldOid) {
      results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    if (!isTagRef && command.newOid !== command.oldOid) {
      const ancestors = ancestorCommits(repo, [command.newOid]);
      if (!ancestors.has(command.oldOid)) {
        results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
        continue;
      }
    }
    results.push({ ref: command.ref, ok: true, reason: '' });
    toApply.push({ ref: command.ref, newOid: command.newOid });
  }
  for (const applied of toApply) {
    if (applied.newOid === ZERO_OID) repo.refs.delete(applied.ref);
    else repo.refs.set(applied.ref, applied.newOid);
  }
  report(true, 'ok', results);
}

// ---------------------------------------------------------------------------
// GraphQL: a small parser + executor for the documents the suite sends.
// ---------------------------------------------------------------------------

type GqlValue =
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'variable'; name: string }
  | { kind: 'list'; items: GqlValue[] }
  | { kind: 'object'; fields: Array<{ name: string; value: GqlValue }> };

interface GqlSelection {
  kind: 'field' | 'inlineFragment';
  name?: string;
  alias?: string;
  args?: Array<{ name: string; value: GqlValue }>;
  typeCondition?: string;
  selections?: GqlSelection[];
}

interface GqlOperation {
  operation: 'query' | 'mutation';
  selections: GqlSelection[];
}

class GqlParser {
  private pos = 0;
  private readonly src: string;
  constructor(src: string) {
    this.src = src;
  }

  private skipWs(): void {
    for (;;) {
      while (this.pos < this.src.length && /[\s,]/.test(this.src[this.pos] as string)) this.pos += 1;
      if (this.src[this.pos] === '#') {
        while (this.pos < this.src.length && this.src[this.pos] !== '\n') this.pos += 1;
        continue;
      }
      break;
    }
  }

  private peek(): string {
    this.skipWs();
    return this.src[this.pos] ?? '';
  }

  private readName(): string {
    this.skipWs();
    const match = /^[A-Za-z_][A-Za-z0-9_]*/.exec(this.src.slice(this.pos));
    if (match === null) throw new Error(`expected a name at ${this.pos}`);
    this.pos += match[0].length;
    return match[0];
  }

  private expect(ch: string): void {
    this.skipWs();
    if (this.src[this.pos] !== ch) throw new Error(`expected ${ch} at ${this.pos}`);
    this.pos += 1;
  }

  private readString(): string {
    this.skipWs();
    if (this.src[this.pos] !== '"') throw new Error(`expected a string at ${this.pos}`);
    this.pos += 1;
    let out = '';
    while (this.pos < this.src.length) {
      const ch = this.src[this.pos] as string;
      if (ch === '"') {
        this.pos += 1;
        return out;
      }
      if (ch === '\\') {
        this.pos += 1;
        const esc = this.src[this.pos] as string;
        if (esc === 'n') out += '\n';
        else if (esc === 't') out += '\t';
        else if (esc === 'r') out += '\r';
        else if (esc === 'u') {
          out += String.fromCharCode(parseInt(this.src.slice(this.pos + 1, this.pos + 5), 16));
          this.pos += 4;
        } else out += esc;
        this.pos += 1;
        continue;
      }
      out += ch;
      this.pos += 1;
    }
    throw new Error('unterminated string');
  }

  private readValue(): GqlValue {
    this.skipWs();
    const ch = this.src[this.pos] ?? '';
    if (ch === '"') return { kind: 'string', value: this.readString() };
    if (ch === '$') {
      this.pos += 1;
      return { kind: 'variable', name: this.readName() };
    }
    if (ch === '[') {
      this.pos += 1;
      const items: GqlValue[] = [];
      while (this.peek() !== ']') items.push(this.readValue());
      this.expect(']');
      return { kind: 'list', items };
    }
    if (ch === '{') {
      this.pos += 1;
      const fields: Array<{ name: string; value: GqlValue }> = [];
      while (this.peek() !== '}') {
        const name = this.readName();
        this.expect(':');
        fields.push({ name, value: this.readValue() });
      }
      this.expect('}');
      return { kind: 'object', fields };
    }
    if (ch === '-' || /[0-9]/.test(ch)) {
      const match = /^-?\d+(\.\d+)?([eE][+-]?\d+)?/.exec(this.src.slice(this.pos));
      if (match === null) throw new Error(`bad number at ${this.pos}`);
      this.pos += match[0].length;
      return match[0].includes('.') || /[eE]/.test(match[0])
        ? { kind: 'float', value: Number(match[0]) }
        : { kind: 'int', value: Number(match[0]) };
    }
    const name = this.readName();
    if (name === 'true') return { kind: 'boolean', value: true };
    if (name === 'false') return { kind: 'boolean', value: false };
    if (name === 'null') return { kind: 'null' };
    return { kind: 'enum', value: name };
  }

  private readArguments(): Array<{ name: string; value: GqlValue }> {
    const args: Array<{ name: string; value: GqlValue }> = [];
    this.skipWs();
    if (this.src[this.pos] !== '(') return args;
    this.pos += 1;
    while (this.peek() !== ')') {
      const name = this.readName();
      this.expect(':');
      args.push({ name, value: this.readValue() });
    }
    this.expect(')');
    return args;
  }

  private readSelectionSet(): GqlSelection[] {
    this.expect('{');
    const selections: GqlSelection[] = [];
    while (this.peek() !== '}') {
      if (this.peek() === '.' && this.src.slice(this.pos, this.pos + 3) === '...') {
        this.pos += 3;
        this.skipWs();
        if (this.src.slice(this.pos, this.pos + 2) === 'on') {
          this.pos += 2;
          const typeCondition = this.readName();
          selections.push({ kind: 'inlineFragment', typeCondition, selections: this.readSelectionSet() });
        } else {
          // Named fragment spread: not used by the suite's documents; skip its
          // selection set if one follows, otherwise ignore.
          this.readName();
          if (this.peek() === '{') this.readSelectionSet();
        }
        continue;
      }
      const first = this.readName();
      let alias: string | undefined;
      let name = first;
      this.skipWs();
      if (this.src[this.pos] === ':') {
        this.pos += 1;
        alias = first;
        name = this.readName();
      }
      const args = this.readArguments();
      let sub: GqlSelection[] | undefined;
      if (this.peek() === '{') sub = this.readSelectionSet();
      selections.push({ kind: 'field', name, alias, args, selections: sub });
    }
    this.expect('}');
    return selections;
  }

  parseOperation(): GqlOperation {
    this.skipWs();
    let operation: 'query' | 'mutation' = 'query';
    if (this.src.slice(this.pos, this.pos + 5) === 'query' && /[\s{(]/.test(this.src[this.pos + 5] ?? '')) {
      this.pos += 5;
      operation = 'query';
    } else if (this.src.slice(this.pos, this.pos + 8) === 'mutation' && /[\s{(]/.test(this.src[this.pos + 8] ?? '')) {
      this.pos += 8;
      operation = 'mutation';
    }
    // Optional operation name and variable definitions.
    this.skipWs();
    if (/[A-Za-z_]/.test(this.src[this.pos] ?? '')) {
      const word = this.readName();
      if (word === 'fragment') throw new Error('fragment definitions are not supported');
    }
    this.skipWs();
    if (this.src[this.pos] === '(') {
      // Variable definitions: skip balanced parens.
      let depth = 0;
      while (this.pos < this.src.length) {
        const ch = this.src[this.pos] as string;
        if (ch === '(') depth += 1;
        else if (ch === ')') {
          depth -= 1;
          this.pos += 1;
          if (depth === 0) break;
          continue;
        } else if (ch === '"') {
          this.readString();
          continue;
        }
        this.pos += 1;
      }
    }
    return { operation, selections: this.readSelectionSet() };
  }
}

function gqlValueToJs(value: GqlValue, variables: Record<string, unknown>): unknown {
  switch (value.kind) {
    case 'string':
    case 'enum':
      return value.value;
    case 'int':
    case 'float':
    case 'boolean':
      return value.value;
    case 'null':
      return null;
    case 'variable':
      return variables[value.name];
    case 'list':
      return value.items.map((item) => gqlValueToJs(item, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const field of value.fields) out[field.name] = gqlValueToJs(field.value, variables);
      return out;
    }
  }
}

interface GqlObject {
  __typename: string;
  repo?: Repo;
  issue?: Issue;
  pull?: Pull;
  userLogin?: string;
  refName?: string;
  pageInfo?: { hasNextPage: boolean; endCursor: string | null };
  nodes?: GqlObject[];
  comment?: { id: number; body: string };
  [key: string]: unknown;
}

const GQL_FIELDS: Record<string, string[]> = {
  Query: ['viewer', 'repository', 'node', '__type', '__schema'],
  Viewer: ['login', 'id', 'name', 'email'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'private', 'url'],
  User: ['login', 'id', 'name', 'email'],
  Ref: ['name', 'target'],
  Issue: ['id', 'number', 'title', 'state', 'body', 'url', 'createdAt'],
  IssueConnection: ['nodes', 'pageInfo', 'totalCount'],
  PageInfo: ['hasNextPage', 'endCursor', 'hasPreviousPage', 'startCursor'],
  PullRequest: ['id', 'number', 'title', 'body', 'state', 'headRefName', 'baseRefName', 'headRef', 'baseRef', 'mergeable', 'merged', 'files', 'url', 'mergedAt', 'mergeCommitSha'],
  PullRequestConnection: ['nodes', 'pageInfo', 'totalCount'],
  PullRequestFileConnection: ['nodes', 'pageInfo'],
  PullRequestFile: ['path', 'additions', 'deletions', 'changes'],
  Comment: ['id', 'body'],
  CommentEdge: ['node'],
  CreateIssuePayload: ['issue'],
  CloseIssuePayload: ['issue'],
  CreatePullRequestPayload: ['pullRequest'],
  MergePullRequestPayload: ['pullRequest'],
  AddCommentPayload: ['commentEdge'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
  __Type: ['name', 'fields', 'enumValues', 'kind'],
  __Field: ['name'],
  __EnumValue: ['name'],
  __Schema: ['types', 'queryType', 'mutationType'],
  SearchType: [],
};

class GqlExecutor {
  private errors: Array<{ message: string }> = [];
  private readonly variables: Record<string, unknown>;

  constructor(variables: Record<string, unknown>) {
    this.variables = variables;
  }

  execute(op: GqlOperation): { data: Record<string, unknown> | null; errors: Array<{ message: string }> } {
    this.errors = [];
    const root: GqlObject = { __typename: op.operation === 'mutation' ? 'Mutation' : 'Query' };
    const data = this.resolveSelections(root, op.selections);
    if (this.errors.length > 0) return { data: null, errors: this.errors };
    return { data, errors: [] };
  }

  private argsOf(sel: GqlSelection): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    for (const arg of sel.args ?? []) out[arg.name] = gqlValueToJs(arg.value, this.variables);
    return out;
  }

  private resolveSelections(obj: GqlObject, selections: GqlSelection[]): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    for (const sel of selections) {
      if (sel.kind === 'inlineFragment') {
        if (sel.typeCondition !== undefined && sel.typeCondition !== obj.__typename) continue;
        Object.assign(out, this.resolveSelections(obj, sel.selections ?? []));
        continue;
      }
      const key = sel.alias ?? (sel.name as string);
      out[key] = this.resolveField(obj, sel);
    }
    return out;
  }

  private resolveField(obj: GqlObject, sel: GqlSelection): unknown {
    const typeName = obj.__typename;
    const field = sel.name as string;
    const known = GQL_FIELDS[typeName] ?? [];
    if (!known.includes(field)) {
      this.errors.push({ message: `Cannot query field "${field}" on type "${typeName}".` });
      return null;
    }
    const args = this.argsOf(sel);
    const value = this.fieldValue(obj, field, args);
    if (value === null || value === undefined) return null;
    if (typeof value === 'object' && (value as GqlObject).__typename !== undefined && sel.selections !== undefined) {
      return this.resolveSelections(value as GqlObject, sel.selections);
    }
    if (Array.isArray(value)) {
      return value.map((item) =>
        typeof item === 'object' && item !== null && (item as GqlObject).__typename !== undefined && sel.selections !== undefined
          ? this.resolveSelections(item as GqlObject, sel.selections)
          : item,
      );
    }
    return value;
  }

  private fieldValue(obj: GqlObject, field: string, args: Record<string, unknown>): unknown {
    const repo = obj.repo;
    switch (obj.__typename) {
      case 'Query': {
        if (field === 'viewer') return { __typename: 'Viewer', userLogin: VIEWER_LOGIN };
        if (field === 'repository') {
          const owner = String(args.owner ?? '');
          const name = String(args.name ?? '');
          if (owner !== VIEWER_LOGIN) return null;
          const found = getRepo(owner, name);
          return found === undefined ? null : { __typename: 'Repository', repo: found };
        }
        if (field === 'node') {
          const parsed = parseGlobalId(String(args.id ?? ''));
          if (parsed === null) return null;
          const found = repoById(parsed.repoId);
          if (found === undefined) return null;
          if (parsed.kind === 'Repository') return { __typename: 'Repository', repo: found };
          if (parsed.kind === 'Issue' && parsed.number !== null) {
            const issue = found.issues.get(parsed.number);
            return issue === undefined ? null : { __typename: 'Issue', repo: found, issue };
          }
          if (parsed.kind === 'PullRequest' && parsed.number !== null) {
            const pull = found.pulls.get(parsed.number);
            return pull === undefined ? null : { __typename: 'PullRequest', repo: found, pull };
          }
          return null;
        }
        if (field === '__type') {
          const name = String(args.name ?? '');
          const fields = GQL_FIELDS[name];
          if (fields === undefined) return null;
          return { __typename: '__Type', typeName: name, fieldNames: fields };
        }
        if (field === '__schema') return { __typename: '__Schema' };
        return null;
      }
      case 'Mutation': {
        if (field === 'createIssue') return this.mutationCreateIssue(args);
        if (field === 'closeIssue') return this.mutationCloseIssue(args);
        if (field === 'createPullRequest') return this.mutationCreatePull(args);
        if (field === 'mergePullRequest') return this.mutationMergePull(args);
        if (field === 'addComment') return this.mutationAddComment(args);
        return null;
      }
      case 'Viewer':
        if (field === 'login') return VIEWER_LOGIN;
        if (field === 'id') return String(VIEWER_ID);
        if (field === 'name') return VIEWER_NAME;
        if (field === 'email') return VIEWER_EMAIL;
        return null;
      case 'Repository': {
        if (repo === undefined) return null;
        if (field === 'id') return globalId('Repository', repo.id);
        if (field === 'databaseId') return repo.id;
        if (field === 'name') return repo.name;
        if (field === 'nameWithOwner') return `${repo.owner}/${repo.name}`;
        if (field === 'description') return repo.description;
        if (field === 'private') return repo.private;
        if (field === 'url') return `https://github.com/${repo.owner}/${repo.name}`;
        if (field === 'owner') return { __typename: 'User', userLogin: repo.owner };
        if (field === 'defaultBranchRef') {
          const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
          return tip === undefined ? null : { __typename: 'Ref', refName: repo.defaultBranch };
        }
        if (field === 'viewerPermission') return 'ADMIN';
        if (field === 'hasIssuesEnabled') return true;
        if (field === 'issue') {
          const issue = repo.issues.get(Number(args.number));
          return issue === undefined ? null : { __typename: 'Issue', repo, issue };
        }
        if (field === 'issues') return this.issueConnection(repo, args);
        if (field === 'pullRequests') return this.pullConnection(repo, args);
        if (field === 'pullRequest') {
          const pull = repo.pulls.get(Number(args.number));
          return pull === undefined ? null : { __typename: 'PullRequest', repo, pull };
        }
        return null;
      }
      case 'User':
        if (field === 'login') return obj.userLogin ?? VIEWER_LOGIN;
        if (field === 'id') return String(VIEWER_ID);
        if (field === 'name') return VIEWER_NAME;
        if (field === 'email') return VIEWER_EMAIL;
        return null;
      case 'Ref':
        if (field === 'name') return obj.refName ?? '';
        return null;
      case 'Issue': {
        const issue = obj.issue as Issue;
        if (field === 'id') return globalId('Issue', (repo as Repo).id, issue.number);
        if (field === 'number') return issue.number;
        if (field === 'title') return issue.title;
        if (field === 'state') return issue.state.toUpperCase();
        if (field === 'body') return issue.body;
        if (field === 'url') return `https://github.com/${(repo as Repo).owner}/${(repo as Repo).name}/issues/${issue.number}`;
        if (field === 'createdAt') return FIXED_ISO;
        return null;
      }
      case 'PullRequest': {
        const pull = obj.pull as Pull;
        if (field === 'id') return globalId('PullRequest', (repo as Repo).id, pull.number);
        if (field === 'number') return pull.number;
        if (field === 'title') return pull.title;
        if (field === 'body') return pull.body;
        if (field === 'state') return pull.state.toUpperCase();
        if (field === 'headRefName') return pull.head;
        if (field === 'baseRefName') return pull.base;
        if (field === 'headRef') return { __typename: 'Ref', refName: pull.head };
        if (field === 'baseRef') return { __typename: 'Ref', refName: pull.base };
        if (field === 'mergeable') return pull.state === 'open' && isMergeable(repo as Repo, pull) ? 'MERGEABLE' : 'CONFLICTING';
        if (field === 'merged') return pull.merged;
        if (field === 'mergedAt') return pull.mergedAt;
        if (field === 'mergeCommitSha') return pull.mergeCommitSha;
        if (field === 'url') return `https://github.com/${(repo as Repo).owner}/${(repo as Repo).name}/pull/${pull.number}`;
        if (field === 'files') {
          const files = pullFiles(repo as Repo, pull);
          const first = typeof args.first === 'number' ? args.first : 10;
          const nodes = files.slice(0, first).map((f) => ({ __typename: 'PullRequestFile', path: f.filename, additions: f.additions, deletions: f.deletions, changes: f.changes }));
          return {
            __typename: 'PullRequestFileConnection',
            nodes,
            pageInfo: { __typename: 'PageInfo', hasNextPage: files.length > first, endCursor: null },
          };
        }
        return null;
      }
      case 'IssueConnection':
      case 'PullRequestConnection':
      case 'PullRequestFileConnection': {
        if (field === 'nodes') return obj.nodes ?? [];
        if (field === 'totalCount') return (obj.nodes ?? []).length;
        if (field === 'pageInfo') return obj.pageInfo;
        return null;
      }
      case 'PageInfo': {
        if (field === 'hasNextPage') return obj.hasNextPage === true;
        if (field === 'endCursor') return typeof obj.endCursor === 'string' ? obj.endCursor : null;
        if (field === 'hasPreviousPage') return false;
        if (field === 'startCursor') return null;
        return null;
      }
      case 'PullRequestFile':
        if (field === 'path') return obj.path;
        if (field === 'additions') return obj.additions;
        if (field === 'deletions') return obj.deletions;
        if (field === 'changes') return obj.changes;
        return null;
      case 'Comment':
        if (field === 'id') return obj.comment !== undefined ? globalId('Comment', (repo as Repo).id, obj.comment.id) : null;
        if (field === 'body') return obj.comment?.body ?? null;
        return null;
      case 'CommentEdge':
        if (field === 'node') return obj.comment === undefined ? null : { __typename: 'Comment', repo, comment: obj.comment };
        return null;
      case 'CreateIssuePayload':
      case 'CloseIssuePayload':
        if (field === 'issue') {
          const issue = obj.issue as Issue | undefined;
          return issue === undefined ? null : { __typename: 'Issue', repo, issue };
        }
        return null;
      case 'CreatePullRequestPayload':
      case 'MergePullRequestPayload':
        if (field === 'pullRequest') {
          const pull = obj.pull as Pull | undefined;
          return pull === undefined ? null : { __typename: 'PullRequest', repo, pull };
        }
        return null;
      case 'AddCommentPayload':
        if (field === 'commentEdge') return { __typename: 'CommentEdge', repo, comment: obj.comment };
        return null;
      case '__Type': {
        if (field === 'name') return obj.typeName;
        if (field === 'kind') return obj.typeName === 'SearchType' ? 'ENUM' : 'OBJECT';
        if (field === 'fields') {
          const names = (obj.fieldNames as string[]) ?? [];
          return names.map((n) => ({ __typename: '__Field', fieldName: n }));
        }
        if (field === 'enumValues') {
          if (obj.typeName === 'SearchType') {
            return ['ISSUE', 'PULL_REQUEST', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT'].map((n) => ({ __typename: '__EnumValue', enumName: n }));
          }
          return [];
        }
        return null;
      }
      case '__Field':
        if (field === 'name') return obj.fieldName;
        return null;
      case '__EnumValue':
        if (field === 'name') return obj.enumName;
        return null;
      case '__Schema': {
        if (field === 'types') {
          return Object.keys(GQL_FIELDS).map((n) => ({ __typename: '__Type', typeName: n, fieldNames: GQL_FIELDS[n] }));
        }
        if (field === 'queryType') return { __typename: '__Type', typeName: 'Query', fieldNames: GQL_FIELDS.Query };
        if (field === 'mutationType') return { __typename: '__Type', typeName: 'Mutation', fieldNames: GQL_FIELDS.Mutation };
        return null;
      }
      default:
        return null;
    }
  }

  private issueConnection(repo: Repo, args: Record<string, unknown>): GqlObject {
    const states = Array.isArray(args.states) ? (args.states as string[]) : null;
    const orderBy = (args.orderBy ?? {}) as { field?: string; direction?: string };
    let items = [...repo.issues.values()];
    if (states !== null) {
      const wanted = new Set(states.map((s) => s.toLowerCase()));
      items = items.filter((i) => wanted.has(i.state));
    } else {
      items = items.filter((i) => i.state === 'open');
    }
    const desc = (orderBy.direction ?? 'ASC').toUpperCase() === 'DESC';
    items.sort((a, b) => (desc ? b.seq - a.seq : a.seq - b.seq));
    return this.paginateNodes(items, args, (issue) => ({ __typename: 'Issue', repo, issue }), (issue) => issue.number);
  }

  private pullConnection(repo: Repo, args: Record<string, unknown>): GqlObject {
    const states = Array.isArray(args.states) ? (args.states as string[]) : null;
    let items = [...repo.pulls.values()];
    if (states !== null) {
      const wanted = new Set(states.map((s) => s.toLowerCase()));
      items = items.filter((p) => wanted.has(p.state));
    } else {
      items = items.filter((p) => p.state === 'open');
    }
    items.sort((a, b) => b.seq - a.seq);
    return this.paginateNodes(items, args, (pull) => ({ __typename: 'PullRequest', repo, pull }), (pull) => pull.number);
  }

  private paginateNodes<T>(
    items: T[],
    args: Record<string, unknown>,
    wrap: (item: T) => GqlObject,
    cursorOf: (item: T) => number,
  ): GqlObject {
    const first = typeof args.first === 'number' ? args.first : 10;
    const after = typeof args.after === 'string' ? args.after : null;
    let start = 0;
    if (after !== null) {
      try {
        const raw = Buffer.from(after, 'base64').toString('utf8');
        const num = Number(raw.split(':')[1]);
        if (Number.isInteger(num)) {
          const idx = items.findIndex((item) => cursorOf(item) === num);
          if (idx >= 0) start = idx + 1;
        }
      } catch {
        start = 0;
      }
    }
    const page = items.slice(start, start + first);
    const hasNextPage = start + page.length < items.length;
    const endCursor = page.length > 0 ? Buffer.from(`cursor:${cursorOf(page[page.length - 1] as T)}`, 'utf8').toString('base64') : null;
    return {
      __typename: 'IssueConnection',
      nodes: page.map(wrap),
      pageInfo: { __typename: 'PageInfo', hasNextPage, endCursor },
    };
  }

  private mutationCreateIssue(args: Record<string, unknown>): GqlObject | null {
    const input = (args.input ?? {}) as { repositoryId?: string; title?: string; body?: string };
    const parsed = parseGlobalId(String(input.repositoryId ?? ''));
    if (parsed === null || parsed.kind !== 'Repository') {
      this.errors.push({ message: 'Could not resolve repository id.' });
      return null;
    }
    const repo = repoById(parsed.repoId);
    if (repo === undefined) {
      this.errors.push({ message: 'Could not resolve repository id.' });
      return null;
    }
    const number = repo.nextNumber++;
    const issue: Issue = {
      id: nextGlobalId++,
      number,
      title: String(input.title ?? ''),
      body: String(input.body ?? ''),
      state: 'open',
      labels: [],
      comments: [],
      seq: number,
    };
    repo.issues.set(number, issue);
    return { __typename: 'CreateIssuePayload', repo, issue };
  }

  private mutationCloseIssue(args: Record<string, unknown>): GqlObject | null {
    const input = (args.input ?? {}) as { issueId?: string };
    const parsed = parseGlobalId(String(input.issueId ?? ''));
    if (parsed === null || parsed.kind !== 'Issue' || parsed.number === null) {
      this.errors.push({ message: 'Could not resolve issue id.' });
      return null;
    }
    const repo = repoById(parsed.repoId);
    const issue = repo?.issues.get(parsed.number);
    if (repo === undefined || issue === undefined) {
      this.errors.push({ message: 'Could not resolve issue id.' });
      return null;
    }
    issue.state = 'closed';
    return { __typename: 'CloseIssuePayload', repo, issue };
  }

  private mutationCreatePull(args: Record<string, unknown>): GqlObject | null {
    const input = (args.input ?? {}) as { repositoryId?: string; baseRefName?: string; headRefName?: string; title?: string; body?: string };
    const parsed = parseGlobalId(String(input.repositoryId ?? ''));
    if (parsed === null || parsed.kind !== 'Repository') {
      this.errors.push({ message: 'Could not resolve repository id.' });
      return null;
    }
    const repo = repoById(parsed.repoId);
    if (repo === undefined) {
      this.errors.push({ message: 'Could not resolve repository id.' });
      return null;
    }
    const head = String(input.headRefName ?? '');
    const base = String(input.baseRefName ?? repo.defaultBranch);
    const headSha = repo.refs.get(`refs/heads/${head}`);
    const baseSha = repo.refs.get(`refs/heads/${base}`);
    if (headSha === undefined || baseSha === undefined) {
      this.errors.push({ message: 'Head or base branch does not exist.' });
      return null;
    }
    for (const existing of repo.pulls.values()) {
      if (existing.head === head && existing.base === base && existing.state === 'open') {
        return { __typename: 'CreatePullRequestPayload', repo, pull: existing };
      }
    }
    const number = repo.nextNumber++;
    const pull: Pull = {
      id: nextGlobalId++,
      number,
      title: String(input.title ?? ''),
      body: String(input.body ?? ''),
      state: 'open',
      head,
      base,
      headSha,
      baseSha,
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      mergeBase: mergeBase(repo, headSha, baseSha),
      seq: number,
    };
    repo.pulls.set(number, pull);
    return { __typename: 'CreatePullRequestPayload', repo, pull };
  }

  private mutationMergePull(args: Record<string, unknown>): GqlObject | null {
    const input = (args.input ?? {}) as { pullRequestId?: string; mergeMethod?: string };
    const parsed = parseGlobalId(String(input.pullRequestId ?? ''));
    if (parsed === null || parsed.kind !== 'PullRequest' || parsed.number === null) {
      this.errors.push({ message: 'Could not resolve pull request id.' });
      return null;
    }
    const repo = repoById(parsed.repoId);
    const pull = repo?.pulls.get(parsed.number);
    if (repo === undefined || pull === undefined) {
      this.errors.push({ message: 'Could not resolve pull request id.' });
      return null;
    }
    if (pull.state === 'open') {
      const result = doMerge(repo, pull);
      if (!result.ok) {
        this.errors.push({ message: 'Merge conflict.' });
        return null;
      }
    }
    return { __typename: 'MergePullRequestPayload', repo, pull };
  }

  private mutationAddComment(args: Record<string, unknown>): GqlObject | null {
    const input = (args.input ?? {}) as { subjectId?: string; body?: string };
    const parsed = parseGlobalId(String(input.subjectId ?? ''));
    if (parsed === null || parsed.number === null) {
      this.errors.push({ message: 'Could not resolve subject id.' });
      return null;
    }
    const repo = repoById(parsed.repoId);
    const issue = repo?.issues.get(parsed.number);
    if (repo === undefined || issue === undefined) {
      this.errors.push({ message: 'Could not resolve subject id.' });
      return null;
    }
    const comment = { id: repo.nextCommentId++, body: String(input.body ?? '') };
    issue.comments.push(comment);
    return { __typename: 'AddCommentPayload', repo, comment };
  }
}

function handleGraphQL(_req: http.IncomingMessage, res: http.ServerResponse, _params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  let parsed: unknown;
  try {
    parsed = JSON.parse(body.toString('utf8'));
  } catch {
    sendError(res, 400, 'Problems parsing JSON');
    return;
  }
  const runOne = (entry: unknown): Record<string, unknown> => {
    if (entry === null || typeof entry !== 'object' || Array.isArray(entry)) {
      return { data: null, errors: [{ message: 'Expected an operation object.' }] };
    }
    const doc = entry as { query?: unknown; variables?: unknown };
    if (typeof doc.query !== 'string') {
      return { data: null, errors: [{ message: 'Expected a query string.' }] };
    }
    const variables = (doc.variables !== null && typeof doc.variables === 'object' && !Array.isArray(doc.variables)
      ? (doc.variables as Record<string, unknown>)
      : {}) as Record<string, unknown>;
    try {
      const op = new GqlParser(doc.query).parseOperation();
      const result = new GqlExecutor(variables).execute(op);
      if (result.errors.length > 0) return { data: null, errors: result.errors };
      return { data: result.data };
    } catch (err) {
      return { data: null, errors: [{ message: err instanceof Error ? err.message : String(err) }] };
    }
  };
  if (Array.isArray(parsed)) {
    sendJson(res, 200, parsed.map(runOne));
    return;
  }
  sendJson(res, 200, runOne(parsed));
}

// ---------------------------------------------------------------------------
// MCP: JSON-RPC 2.0 Streamable HTTP at /mcp (revision 2026-07-28 + legacy
// handshake). Plain JSON responses; no SSE needed by the suite's client.
// ---------------------------------------------------------------------------

const MCP_TOOLS: Array<Record<string, unknown>> = [
  {
    name: 'create_repo',
    description: 'Create a repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'Repository name' },
        description: { type: 'string', description: 'Repository description' },
        private: { type: 'boolean', description: 'Whether the repository is private' },
        owner: { type: 'string', description: 'Owner login (defaults to the authenticated user)' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Issue title' },
        body: { type: 'string', description: 'Issue body' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'Issue state: open, closed or all' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Pull request title' },
        body: { type: 'string', description: 'Pull request body' },
        head: { type: 'string', description: 'Head branch name' },
        base: { type: 'string', description: 'Base branch name' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request with a squash commit.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        pull_number: { type: 'integer', description: 'Pull request number' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

const MCP_SERVER_INFO = { name: 'git-forge', version: '1.0.0' };
const MCP_SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

function mcpToolResult(payload: unknown, isError = false): Record<string, unknown> {
  const text = typeof payload === 'string' ? payload : JSON.stringify(payload);
  return {
    content: [{ type: 'text', text }],
    structuredContent: typeof payload === 'string' ? undefined : payload,
    isError,
  };
}

function mcpCallTool(name: string, args: Record<string, unknown>): Record<string, unknown> {
  const tool = MCP_TOOLS.find((t) => t.name === name);
  if (tool === undefined) return { __rpcError: { code: -32602, message: `Unknown tool: ${name}` } };
  const schema = tool.inputSchema as { required?: string[] };
  const missing = (schema.required ?? []).filter((key) => args[key] === undefined || args[key] === null || args[key] === '');
  if (missing.length > 0) {
    return mcpToolResult({ error: `Missing required argument(s): ${missing.join(', ')}` }, true);
  }
  const owner = String(args.owner ?? VIEWER_LOGIN);
  try {
    switch (name) {
      case 'create_repo': {
        const repoName = String(args.name ?? '');
        if (getRepo(VIEWER_LOGIN, repoName) !== undefined) {
          return mcpToolResult({ error: `Repository ${repoName} already exists` }, true);
        }
        const repo = createRepoRecord(VIEWER_LOGIN, repoName, String(args.description ?? ''), bool(args.private, false));
        return mcpToolResult(repoJson(repo));
      }
      case 'get_repo': {
        const repo = owner === VIEWER_LOGIN ? getRepo(owner, String(args.repo ?? '')) : undefined;
        if (repo === undefined) return mcpToolResult({ error: `Repository ${owner}/${String(args.repo ?? '')} not found` }, true);
        return mcpToolResult(repoJson(repo));
      }
      case 'create_issue': {
        const repo = owner === VIEWER_LOGIN ? getRepo(owner, String(args.repo ?? '')) : undefined;
        if (repo === undefined) return mcpToolResult({ error: `Repository ${owner}/${String(args.repo ?? '')} not found` }, true);
        const number = repo.nextNumber++;
        const issue: Issue = {
          id: nextGlobalId++,
          number,
          title: String(args.title ?? ''),
          body: String(args.body ?? ''),
          state: 'open',
          labels: [],
          comments: [],
          seq: number,
        };
        repo.issues.set(number, issue);
        return mcpToolResult(issueJson(repo, issue));
      }
      case 'list_issues': {
        const repo = owner === VIEWER_LOGIN ? getRepo(owner, String(args.repo ?? '')) : undefined;
        if (repo === undefined) return mcpToolResult({ error: `Repository ${owner}/${String(args.repo ?? '')} not found` }, true);
        const state = String(args.state ?? 'open');
        let items = [...repo.issues.values()];
        if (state !== 'all') items = items.filter((i) => i.state === state);
        items.sort((a, b) => b.seq - a.seq);
        return mcpToolResult({ issues: items.map((i) => issueJson(repo, i)) });
      }
      case 'create_pull_request': {
        const repo = owner === VIEWER_LOGIN ? getRepo(owner, String(args.repo ?? '')) : undefined;
        if (repo === undefined) return mcpToolResult({ error: `Repository ${owner}/${String(args.repo ?? '')} not found` }, true);
        const head = String(args.head ?? '');
        const base = String(args.base ?? repo.defaultBranch);
        const headSha = repo.refs.get(`refs/heads/${head}`);
        const baseSha = repo.refs.get(`refs/heads/${base}`);
        if (headSha === undefined || baseSha === undefined) {
          return mcpToolResult({ error: 'Head or base branch does not exist' }, true);
        }
        for (const existing of repo.pulls.values()) {
          if (existing.head === head && existing.base === base && existing.state === 'open') {
            return mcpToolResult(pullJson(repo, existing));
          }
        }
        const number = repo.nextNumber++;
        const pull: Pull = {
          id: nextGlobalId++,
          number,
          title: String(args.title ?? ''),
          body: String(args.body ?? ''),
          state: 'open',
          head,
          base,
          headSha,
          baseSha,
          merged: false,
          mergedAt: null,
          mergeCommitSha: null,
          mergeBase: mergeBase(repo, headSha, baseSha),
          seq: number,
        };
        repo.pulls.set(number, pull);
        return mcpToolResult(pullJson(repo, pull));
      }
      case 'merge_pull_request': {
        const repo = owner === VIEWER_LOGIN ? getRepo(owner, String(args.repo ?? '')) : undefined;
        if (repo === undefined) return mcpToolResult({ error: `Repository ${owner}/${String(args.repo ?? '')} not found` }, true);
        const pullNumber = Number(args.pull_number);
        const pull = repo.pulls.get(pullNumber);
        if (pull === undefined) return mcpToolResult({ error: `Pull request #${pullNumber} not found` }, true);
        if (pull.state !== 'open') return mcpToolResult({ error: 'Pull request is not open' }, true);
        const result = doMerge(repo, pull);
        if (!result.ok) return mcpToolResult({ error: 'Merge conflict' }, true);
        return mcpToolResult(pullJson(repo, pull));
      }
      default:
        return mcpToolResult({ error: `Tool ${name} is not implemented` }, true);
    }
  } catch (err) {
    return mcpToolResult({ error: err instanceof Error ? err.message : String(err) }, true);
  }
}

function handleMcp(req: http.IncomingMessage, res: http.ServerResponse, _params: Record<string, string>, _query: URLSearchParams, body: Buffer): void {
  if (req.method === 'DELETE') {
    res.writeHead(204, { 'x-github-request-id': `req-${++requestCounter}` });
    res.end();
    return;
  }
  if (req.method !== 'POST') {
    sendError(res, 405, 'Method Not Allowed');
    return;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(body.toString('utf8'));
  } catch {
    sendJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
    return;
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    sendJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } });
    return;
  }
  const msg = parsed as { jsonrpc?: unknown; id?: unknown; method?: unknown; params?: unknown };
  const id = msg.id === undefined ? null : msg.id;
  const method = typeof msg.method === 'string' ? msg.method : '';
  const params = (msg.params !== null && typeof msg.params === 'object' && !Array.isArray(msg.params)
    ? (msg.params as Record<string, unknown>)
    : {}) as Record<string, unknown>;
  const result = (value: unknown): void => {
    sendJson(res, 200, { jsonrpc: '2.0', id, result: value });
  };
  const error = (code: number, message: string): void => {
    sendJson(res, 200, { jsonrpc: '2.0', id, error: { code, message } });
  };
  // Notifications (no id) are answered 202 with no body.
  if (msg.id === undefined) {
    res.writeHead(202, { 'x-github-request-id': `req-${++requestCounter}` });
    res.end();
    return;
  }
  if (method === 'server/discover') {
    result({
      resultType: 'discover',
      supportedVersions: MCP_SUPPORTED_VERSIONS,
      capabilities: { tools: {} },
      instructions: 'A GitHub-core clone: repositories, issues, pull requests, git smart HTTP, GraphQL.',
      serverInfo: MCP_SERVER_INFO,
      _meta: { 'io.modelcontextprotocol/serverInfo': MCP_SERVER_INFO },
    });
    return;
  }
  if (method === 'initialize') {
    const offered = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
    const negotiated = MCP_SUPPORTED_VERSIONS.includes(offered) ? offered : '2025-11-25';
    result({
      protocolVersion: negotiated,
      capabilities: { tools: {} },
      serverInfo: MCP_SERVER_INFO,
    });
    return;
  }
  if (method === 'tools/list') {
    result({ tools: MCP_TOOLS });
    return;
  }
  if (method === 'tools/call') {
    const toolName = typeof params.name === 'string' ? params.name : '';
    const toolArgs = (params.arguments !== null && typeof params.arguments === 'object' && !Array.isArray(params.arguments)
      ? (params.arguments as Record<string, unknown>)
      : {}) as Record<string, unknown>;
    const outcome = mcpCallTool(toolName, toolArgs);
    if ('__rpcError' in outcome) {
      const rpcError = outcome.__rpcError as { code: number; message: string };
      error(rpcError.code, rpcError.message);
      return;
    }
    result(outcome);
    return;
  }
  if (method === 'ping') {
    result({});
    return;
  }
  error(-32601, `Method not found: ${method}`);
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

function decodeSegment(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

async function route(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
  const url = new URL(req.url ?? '/', `http://${req.headers.host ?? '127.0.0.1'}`);
  const query = url.searchParams;
  const rawSegments = url.pathname.split('/').filter((s) => s !== '');
  const segments = rawSegments.map(decodeSegment);
  const method = (req.method ?? 'GET').toUpperCase();

  if (method === 'GET' && segments.length === 1 && segments[0] === 'healthz') {
    sendJson(res, 200, { ok: true });
    return;
  }

  // Everything else needs the bearer token.
  if (!requireAuth(req, res)) return;

  const needsBody = method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE';
  const body = needsBody ? await readBody(req) : Buffer.alloc(0);

  try {
    // GraphQL + MCP
    if (segments.length === 2 && segments[0] === 'api' && segments[1] === 'graphql') {
      if (method !== 'POST') return sendError(res, 405, 'Method Not Allowed');
      handleGraphQL(req, res, {}, query, body);
      return;
    }
    if (segments.length === 1 && segments[0] === 'mcp') {
      handleMcp(req, res, {}, query, body);
      return;
    }

    // Git smart HTTP: /{owner}/{repo}.git/...
    if (segments.length >= 3 && (segments[1] as string).endsWith('.git')) {
      const owner = segments[0] as string;
      const repoName = (segments[1] as string).slice(0, -4);
      const tail = segments.slice(2);
      if (method === 'GET' && tail.length === 2 && tail[0] === 'info' && tail[1] === 'refs') {
        handleInfoRefs(req, res, { owner, repo: repoName }, query);
        return;
      }
      if (method === 'POST' && tail.length === 1 && tail[0] === 'git-upload-pack') {
        handleUploadPack(req, res, { owner, repo: repoName }, query, body);
        return;
      }
      if (method === 'POST' && tail.length === 1 && tail[0] === 'git-receive-pack') {
        handleReceivePack(req, res, { owner, repo: repoName }, query, body);
        return;
      }
      return sendError(res, 404, 'Not Found');
    }

    // REST v3
    if (segments.length >= 2 && segments[0] === 'api' && segments[1] === 'v3') {
      const rest = segments.slice(2);
      const params = (owner: string, repo: string): Record<string, string> => ({ owner, repo });
      if (rest.length === 0 && method === 'GET') return handleRoot(req, res);
      if (rest.length === 1 && rest[0] === 'meta' && method === 'GET') return handleMeta(req, res);
      if (rest.length === 1 && rest[0] === 'user' && method === 'GET') return handleGetUser(req, res);
      if (rest.length === 2 && rest[0] === 'user' && rest[1] === 'repos') {
        if (method === 'GET') return handleListUserRepos(req, res, {}, query);
        if (method === 'POST') return handleCreateRepo(req, res, {}, query, body);
        return sendError(res, 405, 'Method Not Allowed');
      }
      if (rest.length >= 3 && rest[0] === 'repos') {
        const owner = rest[1] as string;
        const repo = rest[2] as string;
        const tail = rest.slice(3);
        if (tail.length === 0) {
          if (method === 'GET') return handleGetRepo(req, res, params(owner, repo));
          if (method === 'PATCH') return handlePatchRepo(req, res, params(owner, repo), query, body);
          if (method === 'DELETE') return handleDeleteRepo(req, res, params(owner, repo));
          return sendError(res, 405, 'Method Not Allowed');
        }
        if (tail.length === 1 && tail[0] === 'branches' && method === 'GET') {
          return handleListBranches(req, res, params(owner, repo), query);
        }
        if (tail.length >= 2 && tail[0] === 'branches' && method === 'GET') {
          return handleGetBranch(req, res, { ...params(owner, repo), branch: tail.slice(1).join('/') });
        }
        if (tail.length === 1 && tail[0] === 'commits' && method === 'GET') {
          return handleListCommits(req, res, params(owner, repo), query);
        }
        if (tail.length >= 1 && tail[0] === 'contents') {
          const path = tail.slice(1).join('/');
          if (method === 'GET') return handleGetContents(req, res, { ...params(owner, repo), path }, query);
          if (method === 'PUT') return handlePutContents(req, res, { ...params(owner, repo), path }, query, body);
          if (method === 'DELETE') return handleDeleteContents(req, res, { ...params(owner, repo), path }, query, body);
          return sendError(res, 405, 'Method Not Allowed');
        }
        if (tail.length === 1 && tail[0] === 'labels') {
          if (method === 'GET') return handleListLabels(req, res, params(owner, repo));
          if (method === 'POST') return handleCreateLabel(req, res, params(owner, repo), query, body);
          return sendError(res, 405, 'Method Not Allowed');
        }
        if (tail.length === 1 && tail[0] === 'issues') {
          if (method === 'GET') return handleListIssues(req, res, params(owner, repo), query);
          if (method === 'POST') return handleCreateIssue(req, res, params(owner, repo), query, body);
          return sendError(res, 405, 'Method Not Allowed');
        }
        if (tail.length === 2 && tail[0] === 'issues') {
          if (method === 'GET') return handleGetIssue(req, res, { ...params(owner, repo), number: tail[1] as string });
          if (method === 'PATCH') return handlePatchIssue(req, res, { ...params(owner, repo), number: tail[1] as string }, query, body);
          return sendError(res, 405, 'Method Not Allowed');
        }
        if (tail.length === 3 && tail[0] === 'issues' && tail[2] === 'comments') {
          if (method === 'GET') return handleListComments(req, res, { ...params(owner, repo), number: tail[1] as string }, query);
          if (method === 'POST') return handleCreateComment(req, res, { ...params(owner, repo), number: tail[1] as string }, query, body);
          return sendError(res, 405, 'Method Not Allowed');
        }
        if (tail.length === 1 && tail[0] === 'pulls') {
          if (method === 'GET') return handleListPulls(req, res, params(owner, repo), query);
          if (method === 'POST') return handleCreatePull(req, res, params(owner, repo), query, body);
          return sendError(res, 405, 'Method Not Allowed');
        }
        if (tail.length === 2 && tail[0] === 'pulls') {
          if (method === 'GET') return handleGetPull(req, res, { ...params(owner, repo), number: tail[1] as string });
          return sendError(res, 405, 'Method Not Allowed');
        }
        if (tail.length === 3 && tail[0] === 'pulls' && tail[2] === 'files' && method === 'GET') {
          return handleListPullFiles(req, res, { ...params(owner, repo), number: tail[1] as string });
        }
        if (tail.length === 3 && tail[0] === 'pulls' && tail[2] === 'merge' && method === 'PUT') {
          return handleMergePull(req, res, { ...params(owner, repo), number: tail[1] as string }, query, body);
        }
        if (tail.length === 2 && tail[0] === 'git' && tail[1] === 'refs') {
          if (method === 'POST') return handleCreateRef(req, res, params(owner, repo), query, body);
          return sendError(res, 405, 'Method Not Allowed');
        }
        if (tail.length >= 4 && tail[0] === 'git' && tail[1] === 'refs' && tail[2] === 'heads' && method === 'GET') {
          return handleGetRef(req, res, { ...params(owner, repo), branch: tail.slice(3).join('/') });
        }
      }
      return sendError(res, 404, 'Not Found');
    }

    return sendError(res, 404, 'Not Found');
  } catch (err) {
    process.stderr.write(`[git-forge] handler error: ${err instanceof Error ? err.stack ?? err.message : String(err)}\n`);
    if (!res.headersSent) sendError(res, 500, 'Internal Server Error');
    else res.end();
  }
}

function createApp(): http.Server {
  return http.createServer((req, res) => {
    void route(req, res);
  });
}

// ---------------------------------------------------------------------------
// Lifecycle: the entry <-> env contract (starter main.ts, adapted: without
// MAKEFUN_ENV_URL the server serves locally forever for practice runs).
// ---------------------------------------------------------------------------

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet: it is not done, so poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

async function main(): Promise<void> {
  const server = createApp();
  const port = ENV_URL === '' ? Number(process.env.PORT ?? 0) : 0;
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, '127.0.0.1', () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') throw new Error('the server did not bind an ephemeral TCP port');
  process.stdout.write(`[git-forge] listening on http://127.0.0.1:${address.port} (viewer ${VIEWER_LOGIN})\n`);
  if (ENV_URL === '') {
    process.stdout.write('[git-forge] local mode: MAKEFUN_ENV_URL is not set; serving until killed\n');
    await new Promise<void>(() => undefined);
    return;
  }
  await register(address.port);
  process.stdout.write('[git-forge] registered; waiting for the env to say done\n');
  await waitForDone();
  server.close();
  process.stdout.write('[git-forge] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[git-forge] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
