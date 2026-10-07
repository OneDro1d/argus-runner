const fastify = require('fastify')();

fastify.register(function (instance, opts, done) {
  instance.get('/list', async (req, reply) => {
    return { ok: true };
  });
  instance.route({ method: 'GET', url: '/status' });
  done();
}, { prefix: '/items' });

fastify.get('/health', async (req, reply) => {
  return { status: 'ok' };
});

module.exports = fastify;
