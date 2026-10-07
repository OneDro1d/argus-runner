const app = require("express")();
app.get("/users/:id", function (req, res) {
  res.json({ id: req.params.id });
});
module.exports = app;
