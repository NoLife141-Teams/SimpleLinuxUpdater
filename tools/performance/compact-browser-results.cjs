// Publish numeric evidence and request/byte counts, keeping cookie-bearing
// Playwright traces and bulky per-request CPU profiles local.
const fs = require('node:fs');
const crypto = require('node:crypto');
const input = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const route = pathname => pathname.replace(/\/api\/jobs\/[^/]+\/logs.*/, '/api/jobs/:id/logs');
const countByRoute = requests => requests.reduce((all, value) => { const key = route(value.split('?')[0]); all[key] = (all[key] || 0) + 1; return all; }, {});
const samples = input.samples.map(sample => {
  const bytes = {};
  for (const resource of sample.resources) {
    const key = route(resource.path);
    const row = bytes[key] ||= { count: 0, encoded_bytes: 0, decoded_bytes: 0 };
    row.count++; row.encoded_bytes += resource.encoded_bytes; row.decoded_bytes += resource.decoded_bytes;
  }
  const maxConcurrent = {};
  for (const [key, value] of Object.entries(sample.max_concurrent_by_path)) maxConcurrent[route(key)] = Math.max(maxConcurrent[route(key)] || 0, value);
  return {
    ...Object.fromEntries(['initial', 'filter', 'select', 'detail', 'refresh', 'reconnects', 'sse_connections', 'hidden_selection_hint', 'metrics', 'dom_nodes', 'rows'].map(key => [key, sample[key]])),
    requests: countByRoute(sample.requests), bytes, max_concurrent_by_path: maxConcurrent,
    long_tasks: { count: sample.long_tasks_ms.length, total_ms: sample.long_tasks_ms.reduce((sum, value) => sum + value, 0), max_ms: Math.max(0, ...sample.long_tasks_ms) },
    reconnect_samples: sample.reconnect_samples.map(cycle => ({ requests: countByRoute(cycle.requests), target_count: cycle.targets.length, targets_sha256: crypto.createHash('sha256').update(JSON.stringify(cycle.targets)).digest('hex'), first_targets: cycle.targets.slice(0, 3) })),
  };
});
console.log(JSON.stringify({ ...input, samples, source_json_sha256: crypto.createHash('sha256').update(fs.readFileSync(process.argv[2])).digest('hex'), scope: 'Compact numeric evidence; all five measured samples retained. Raw traces and complete per-request records remain local.' }, null, 2));
