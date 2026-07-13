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
