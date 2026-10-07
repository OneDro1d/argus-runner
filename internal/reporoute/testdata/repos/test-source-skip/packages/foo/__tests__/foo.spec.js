const app = require("express")();
app.get("/fixture-only-route", function (req, res) {
  res.json({ ok: true });
});
module.exports = app;
