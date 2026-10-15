// Private run view shared by the socket API and the signed-in dashboard.
//
// Command runs are log-only by default. Some command jobs are wrappers that
// launch a Claude session themselves; `scheduler join` can resume such a run
// when the view carries `continuation: { kind: 'claude', sessionId, cwd }`,
// or refuse it with a `continuationBlocked` reason. That mapping must come
// from evidence the wrapper wrote for this exact run, validated on every read,
// so it is site-specific: extend wrapperContinuation for your own wrappers.
// The default derives nothing.
export function wrapperContinuation(run) {
  return {};
}

// Called through Array#map, so it takes exactly one argument.
export function privateRun(run) {
  // Never trust cached derived fields; derive them again on every read.
  const { continuation, continuationBlocked, wrappedAgent, ...saved } = run;
  return { ...saved, ...wrapperContinuation(saved) };
}
