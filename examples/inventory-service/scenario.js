import http from 'k6/http';
import { check, sleep } from 'k6';

const state = JSON.parse(open(__ENV.STATE_FILE));
const baseURL = __ENV.BASE_URL || 'http://localhost:8080';

// Ramping stages (rather than a flat --vus/--duration) so load — and the
// derived metrics it drives on the stub (queue depth, latency, stock,
// errors) — visibly rises and falls over the run instead of sitting flat.
// k6 CLI flags override script options, so myrtille.yaml intentionally
// passes no --vus/--duration for this example and lets these stages run.
export const options = {
  stages: [
    { duration: '5s', target: 20 },
    { duration: '10s', target: 20 },
    { duration: '5s', target: 2 },
    { duration: '5s', target: 2 },
  ],
  thresholds: {
    http_req_failed: ['rate<0.5'],
  },
};

export default function () {
  const sku = state.product_ids[Math.floor(Math.random() * state.product_ids.length)];
  const qty = 1 + Math.floor(Math.random() * 3);

  const res = http.post(
    `${baseURL}/orders`,
    JSON.stringify({ sku, qty }),
    { headers: { 'Content-Type': 'application/json' } }
  );

  check(res, { 'status is 201 or 503': (r) => r.status === 201 || r.status === 503 });
  sleep(0.1);
}
