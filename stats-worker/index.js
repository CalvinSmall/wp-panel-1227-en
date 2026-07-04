// WP Panel Anonymous Installation Statistics Worker
// POST /api/heartbeat — Panel anonymous heartbeat reporting
// GET  /api/stats     — Public statistics (total + precise active_24h rolling window)

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const corsHeaders = {
      'Access-Control-Allow-Origin': '*',
      'Access-Control-Allow-Methods': 'GET, POST, OPTIONS',
      'Access-Control-Allow-Headers': 'Content-Type',
    };

    if (request.method === 'OPTIONS') {
      return new Response(null, { headers: corsHeaders });
    }

    // Public statistics — precisely calculate last 24-hour active count by id:* last timestamp
    if (request.method === 'GET' && url.pathname === '/api/stats') {
      const stats = await getStats(env);
      return new Response(JSON.stringify(stats), {
        headers: {
          ...corsHeaders,
          'Content-Type': 'application/json',
          'Cache-Control': 'public, max-age=300',
        },
      });
    }

    // Anonymous heartbeat — panel periodic reporting
    if (request.method === 'POST' && url.pathname === '/api/heartbeat') {
      try {
        const body = await request.json();
        const { anonymous_id, version } = body;
        if (!anonymous_id || typeof anonymous_id !== 'string' || anonymous_id.length < 8) {
          return new Response(JSON.stringify({ error: 'invalid anonymous_id' }), {
            status: 400,
            headers: { ...corsHeaders, 'Content-Type': 'application/json' },
          });
        }
        await saveHeartbeat(env, anonymous_id, version || 'unknown');
        return new Response(JSON.stringify({ ok: true }), {
          headers: { ...corsHeaders, 'Content-Type': 'application/json' },
        });
      } catch {
        return new Response(JSON.stringify({ error: 'invalid request' }), {
          status: 400,
          headers: { ...corsHeaders, 'Content-Type': 'application/json' },
        });
      }
    }

    // Legacy deployment compatibility: rebuild total snapshot. /api/stats no longer depends on this counter.
    if (request.method === 'POST' && url.pathname === '/api/migrate') {
      const stats = await getStats(env);
      await env.STATS_KV.put('meta:total', String(stats.total));
      return new Response(JSON.stringify({ migrated: true }), {
        headers: { ...corsHeaders, 'Content-Type': 'application/json' },
      });
    }

    return new Response('Not Found', { status: 404 });
  },
};

async function getStats(env) {
  let total = 0;
  let active = 0;
  const cutoff = Date.now() - 24 * 60 * 60 * 1000;

  let cursor;
  do {
    const result = await env.STATS_KV.list({ prefix: 'id:', cursor, limit: 1000 });
    total += result.keys.length;

    const keysWithoutMetadata = [];
    for (const key of result.keys) {
      const last = key.metadata?.last;
      if (!last) {
        keysWithoutMetadata.push(key);
        continue;
      }
      const lastTime = Date.parse(last);
      if (Number.isFinite(lastTime) && lastTime >= cutoff) {
        active++;
      }
    }

    for (let i = 0; i < keysWithoutMetadata.length; i += 50) {
      const batch = keysWithoutMetadata.slice(i, i + 50);
      const records = await Promise.all(batch.map(async key => {
        try {
          return await env.STATS_KV.get(key.name, { type: 'json' });
        } catch {
          return null;
        }
      }));

      for (const data of records) {
        const lastTime = data?.last ? Date.parse(data.last) : NaN;
        if (Number.isFinite(lastTime) && lastTime >= cutoff) {
          active++;
        }
      }
    }

    cursor = result.list_complete ? undefined : result.cursor;
  } while (cursor);

  return {
    total,
    active_24h: active,
    active: active,
  };
}

// Write heartbeat: each anonymous instance retains first and most recent reporting time.
async function saveHeartbeat(env, anonymousId, version) {
  const now = new Date().toISOString();
  const idKey = `id:${anonymousId}`;

  const existing = await env.STATS_KV.get(idKey, { type: 'json' });

  const writes = [];

  writes.push(env.STATS_KV.put(
    idKey,
    JSON.stringify({
      v: version,
      first: existing?.first || now,
      last: now,
    }),
    { metadata: { last: now } }
  ));

  // New installation → total +1
  if (!existing) {
    const total = parseInt(await env.STATS_KV.get('meta:total')) || 0;
    writes.push(env.STATS_KV.put('meta:total', String(total + 1)));
  }

  await Promise.all(writes);
}
