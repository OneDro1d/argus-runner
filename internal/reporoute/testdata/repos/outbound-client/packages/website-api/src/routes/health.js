const app = require("express")();
app.get("/health", function (req, res) {
  res.json({ status: "ok" });
});
module.exports = app;
