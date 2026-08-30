import http from 'k6/http';
import { check, sleep } from 'k6';

const state = JSON.parse(open(__ENV.STATE_FILE));
const baseURL = __ENV.BASE_URL || 'http://localhost:8080';

export const options = {
  thresholds: {
    http_req_failed: ['rate<0.01'],
  },
};

export default function () {
  const userId = state.user_ids[Math.floor(Math.random() * state.user_ids.length)];
  const productId = state.product_ids[Math.floor(Math.random() * state.product_ids.length)];

  const res = http.post(
    `${baseURL}/orders`,
    JSON.stringify({ userId, productId }),
    { headers: { 'Content-Type': 'application/json' } }
  );

  check(res, { 'status is 201': (r) => r.status === 201 });
  sleep(0.2);
}
