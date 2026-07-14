const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const source = fs.readFileSync(path.join(__dirname, "app.js"), "utf8").replace(/\nboot\(\);\s*$/, "\n");
const context = vm.createContext({
  console,
  clearInterval,
  clearTimeout,
  fetch: async () => { throw new Error("unexpected fetch"); },
  Intl,
  setInterval,
  setTimeout,
});
vm.runInContext(source, context);

test("current orders render asks above bids with best prices next to the spread", () => {
  const html = context.ordersTable([
    order("ask-101", "SELL", "101"),
    order("bid-98", "BUY", "98"),
    order("ask-103", "SELL", "103"),
    order("bid-99", "BUY", "99"),
    order("ask-102", "SELL", "102"),
    order("bid-97", "BUY", "97"),
  ]);

  assertOrdered(html, [
    "卖盘",
    "订单 ask-103",
    "订单 ask-102",
    "订单 ask-101",
    "order-book-mid",
    "买盘",
    "订单 bid-99",
    "订单 bid-98",
    "订单 bid-97",
  ]);
  assert.match(html, /最低卖价[\s\S]*101/);
  assert.match(html, /最高买价[\s\S]*99/);
});

test("current orders no longer truncate after twenty rows", () => {
  const orders = Array.from({ length: 25 }, (_, index) => order(`bid-${index + 1}`, "BUY", String(index + 1)));
  const html = context.ordersTable(orders);
  assert.match(html, /订单 bid-1/);
  assert.match(html, /订单 bid-25/);
  assert.match(html, /25 笔/);
});

test("price sorting preserves precision for small decimal ticks", () => {
  assert.equal(context.compareDecimalPrices("0.000000000000000002", "0.000000000000000001"), 1);
  assert.equal(context.compareDecimalPrices("100.0100", "100.01"), 0);
});

test("quote sizing accepts a valid quote-asset notional range", () => {
  assert.equal(context.strategyOrderSizingIssue({ min_order_notional: "10", max_order_notional: "20" }), "");
  assert.match(context.strategyOrderSizingIssue({ min_order_notional: "20", max_order_notional: "10" }), /最大金额/);
  assert.match(context.strategyOrderSizingIssue({ min_order_notional: "10" }), /必须都大于/);
});

test("legacy fixed quantity remains valid until a notional range is configured", () => {
  assert.equal(context.strategyOrderSizingIssue({ order_size: "1" }), "");
  assert.match(context.strategyOrderSizingIssue({}), /最小和最大金额/);
});

test("strategy page renders progressive quote refresh controls and defaults", () => {
  vm.runInContext(`
    state.user = { permissions: ["strategy:edit", "config:edit"] };
    state.draft = { venues: {} };
  `, context);
  const html = context.strategyTab({
    id: "btc_usdt",
    base: { symbol: "BTC" },
    quote: { symbol: "USDT" },
    strategy: {
      levels: 20,
      min_order_notional: "10",
      max_order_notional: "20",
    },
  }, 0);

  assert.match(html, /盘口渐进轮换/);
  assert.match(html, /data-path="instruments\.0\.strategy\.quote_refresh_seconds"[^>]*value="45"/);
  assert.match(html, /data-path="instruments\.0\.strategy\.quote_refresh_ratio_bps"[^>]*value="1000"/);
  assert.match(html, /data-path="instruments\.0\.strategy\.min_order_lifetime_seconds"[^>]*value="30"/);
  assert.match(html, /data-path="instruments\.0\.strategy\.max_order_lifetime_seconds"[^>]*value="300"/);
  assert.match(html, /data-path="instruments\.0\.strategy\.price_jitter_ticks"[^>]*value="2"/);
  assert.match(html, /data-path="instruments\.0\.strategy\.best_levels"[^>]*value="3"/);
  assert.match(html, /data-path="instruments\.0\.strategy\.best_level_refresh_seconds"[^>]*value="90"/);
  assert.match(html, /10～20 USDT/);
});

test("runtime book labels distinguish empty and one-sided maker bootstrap", () => {
  assert.deepEqual(
    JSON.parse(JSON.stringify(context.bookDisplayState({ bid_price: "0", ask_price: "0" }))),
    { hasBid: false, hasAsk: false, twoSided: false, label: "空盘口 · 按指数价铺单" },
  );
  assert.equal(context.bookDisplayState({ bid_price: "1", ask_price: "0" }).label, "单边盘口 · 按指数价补单");
  assert.equal(context.bookDisplayState(null).label, "盘口接口不可用 · 按指数价铺单");
});

function order(id, side, price) {
  return { order_id: id, side, price, quantity: "1", executed_qty: "0", state: "NEW" };
}

function assertOrdered(value, parts) {
  let previous = -1;
  for (const part of parts) {
    const index = value.indexOf(part, previous + 1);
    assert.notEqual(index, -1, `missing ${part}`);
    assert.ok(index > previous, `${part} is out of order`);
    previous = index;
  }
}
