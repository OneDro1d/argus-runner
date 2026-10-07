const app = require('express')();

app.post('/api/v1/orders', function (req, res) {
  res.status(201).json({ order_id: '123' });
});

app.get('/api/v1/quote', function (req, res) {
  res.json({ price: 42 });
});

app.get('/api/v1/health', function (req, res) {
  res.json({ status: 'ok' });
});

module.exports = app;
