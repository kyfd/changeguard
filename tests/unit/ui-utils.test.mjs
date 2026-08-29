import assert from "node:assert/strict";
import test from "node:test";

import { activateOnKeyboard } from "../../internal/httpapi/web/ui-utils.mjs";

test("activateOnKeyboard invokes click for Enter and Space", () => {
  let clicks = 0;
  const target = { click: () => { clicks += 1; } };
  for (const key of ["Enter", " "]) {
    activateOnKeyboard({ key, currentTarget: target, preventDefault() {} });
  }
  assert.equal(clicks, 2);
});

test("activateOnKeyboard ignores unrelated keys", () => {
  let clicks = 0;
  activateOnKeyboard({ key: "Escape", currentTarget: { click: () => { clicks += 1; } }, preventDefault() {} });
  assert.equal(clicks, 0);
});
