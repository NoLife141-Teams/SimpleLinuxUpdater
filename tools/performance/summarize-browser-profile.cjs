const fs = require('node:fs');
const profile = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const nodes = new Map(profile.nodes.map(node => [node.id, node]));
const parents = new Map();
for (const node of nodes.values()) for (const child of node.children || []) parents.set(child, node.id);
const totals = new Map();
for (let index = 0; index < profile.samples.length; index++) {
  let id = profile.samples[index];
  const microseconds = profile.timeDeltas[index];
  let self = true;
  const seen = new Set();
  while (id !== undefined) {
    const frame = nodes.get(id).callFrame;
    const url = frame.url ? new URL(frame.url).pathname : '';
    const key = `${frame.functionName || '(anonymous)'} ${url}:${frame.lineNumber + 1}`;
    if (!seen.has(key)) {
      const total = totals.get(key) || { frame: key, self_ms: 0, inclusive_ms: 0 };
      total.inclusive_ms += microseconds / 1000;
      if (self) total.self_ms += microseconds / 1000;
      totals.set(key, total); seen.add(key);
    }
    self = false; id = parents.get(id);
  }
}
const app = [...totals.values()].filter(item => item.frame.includes('/static/js/'));
console.log(JSON.stringify({ interval_us: 1000, duration_ms: (profile.endTime - profile.startTime) / 1000, self: app.toSorted((a, b) => b.self_ms - a.self_ms).slice(0, 25), inclusive: app.toSorted((a, b) => b.inclusive_ms - a.inclusive_ms).slice(0, 25) }, null, 2));
