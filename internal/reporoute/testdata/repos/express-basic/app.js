const express = require('express');
const app = express();
const router = express.Router();

router.get('/users/:id', function (req, res) {
  res.json({ id: req.params.id });
});

app.post('/users', function (req, res) {
  res.status(201).json({ created: true });
});

// Not routes: a Map/URLSearchParams .get() shares Fastify/Express's exact call shape but never
// starts with "/" — a real repo (shop-services) proposed these as fake "routes" before
// this guard existed.
function readAction(params) {
  const action = params.get('action');
  const cache = new Map();
  const amount = cache.get('amount');
  return action + amount;
}

module.exports = app;
