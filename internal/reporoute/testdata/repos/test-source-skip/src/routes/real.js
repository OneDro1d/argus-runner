const app = require("express")();
app.get("/real", function (req, res) {
  res.json({ ok: true });
});
module.exports = app;
