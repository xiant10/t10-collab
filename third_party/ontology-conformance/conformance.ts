/**
 * Black-box tests of the ontology contract (README.md). It speaks only HTTP and MCP to a module, so a module in any
 * language can run it, with three test tokens and a config naming the module's own fixtures:
 *
 *   node contracts/ontology/conformance.ts <module URL> <config.json>
 *   node contracts/ontology/conformance.ts --self-test   # against t10-modkit's stub, unbroken and broken each way
 *
 * The config, such as testdata/stub.json, gives:
 *   tokens: in_scope (a person who sees `visible`), out_of_scope (a person in the same tenant who doesn't), agent (a
 *     delegated token for in_scope's person whose grant holds <prefix>:read and <prefix>:propose), and optionally
 *     agent_read_only (one whose grant holds only <prefix>:read), without which the grant check is skipped;
 *   visible: a reference in_scope sees;
 *   search: {type, body, limit, expect}: a search in_scope sees `expect` objects of, paged by `limit`;
 *   forbidden, wrong_state, apply: {action, object, params}: an action in_scope sees but may not take, one on an
 *     object in a state that refuses it, and a repeatable one;
 *   idempotent: an idempotent action; undo: an undo action, with `dependent`, an action that then depends on it. A
 *     module with none leaves them out, and those checks are skipped. A module where nothing can depend on an undo
 *     action leaves out `dependent`, and the undo_conflict check is skipped;
 *   sensitive: optionally, a reference in_scope sees with a value in a sensitive property, for the masking checks.
 *     Without it the suite looks for one with in_scope's search of each type that has a sensitive property;
 *   mcp: optionally, the path of the module's MCP endpoint, /mcp when left out.
 * An object may be "$first:<type>", the first object in_scope's search finds, or "$newest:<type>", the same, read
 * when it's used. The self-test needs Go and the t10-modkit checkout beside this workspace.
 */
import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { checkDescriptor } from "./descriptor.ts";

type Json = any;
type ActionCase = { action: string; object?: string; params: Record<string, Json> };
export type Config = {
	tokens: { in_scope: string; out_of_scope: string; agent: string; agent_read_only?: string };
	visible: string;
	search: { type: string; body: Record<string, Json>; limit: number; expect: number };
	forbidden: ActionCase;
	wrong_state: ActionCase;
	apply: ActionCase;
	idempotent?: ActionCase;
	undo?: ActionCase & { dependent?: ActionCase };
	sensitive?: string;
	mcp?: string;
};

class Failure extends Error {}
const must = (ok: unknown, message: string) => {
	if (!ok) throw new Failure(message);
};

type Answer = { status: number; body: Json; text: string };

/** Runs every check, and answers the names of those that failed, each with why. */
export async function runSuite(base: string, config: Config, log: (line: string) => void = () => {}): Promise<string[]> {
	const call = async (method: string, path: string, token?: string, body?: unknown, headers: Record<string, string> = {}): Promise<Answer> => {
		const h: Record<string, string> = { ...headers };
		if (token) h.authorization = `Bearer ${token}`;
		if (body !== undefined) h["content-type"] = "application/json";
		const res = await fetch(base + path, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body) });
		const text = await res.text();
		let parsed: Json;
		try {
			parsed = JSON.parse(text);
		} catch {
			parsed = undefined;
		}
		return { status: res.status, body: parsed, text };
	};
	const { in_scope: person, out_of_scope: outsider, agent } = config.tokens;
	const split = (ref: string) => {
		const [module, type, ...rest] = ref.split(":");
		return { module, type, id: rest.join(":") };
	};
	const objectPath = (ref: string) => `/v1/objects/${encodeURIComponent(split(ref).type)}/${encodeURIComponent(split(ref).id)}`;
	const resolveRef = async (ref: string | undefined): Promise<string | undefined> => {
		const m = ref?.match(/^\$(first|newest):(\w+)$/);
		if (!m) return ref;
		const found = await call("POST", `/v1/objects/${m[2]}/search`, person, { limit: 1 });
		must(found.status === 200 && found.body.objects.length > 0, `${ref}: in_scope's search of ${m[2]} found nothing`);
		return found.body.objects[0].ref;
	};
	const body = async (c: ActionCase, revision?: number) => {
		const object = await resolveRef(c.object);
		const params: Record<string, Json> = {};
		for (const [k, v] of Object.entries(c.params)) params[k] = typeof v === "string" && v.startsWith("$") ? await resolveRef(v) : v;
		let rev = revision;
		if (rev === undefined && object) {
			const got = await call("GET", objectPath(object), person);
			must(got.status === 200, `${object} isn't visible to in_scope (${got.status})`);
			rev = got.body.revision;
		}
		return { object, revision: rev, params };
	};
	const apply = async (c: ActionCase, token = person, key?: string, revision?: number) =>
		call("POST", `/v1/actions/${encodeURIComponent(c.action)}/apply`, token, await body(c, revision), key ? { "idempotency-key": key } : {});

	let manifest: Json;
	let descriptor: Json;
	const failed: string[] = [];
	const check = async (name: string, run: () => Promise<void>, skip?: string) => {
		if (skip) {
			log(`skip ${name}: ${skip}`);
			return;
		}
		try {
			await run();
			log(`ok   ${name}`);
		} catch (e) {
			const why = e instanceof Error ? e.message : String(e);
			failed.push(`${name}: ${why}`);
			log(`FAIL ${name}: ${why}`);
		}
	};

	await check("manifest lists ontology", async () => {
		const m = await call("GET", "/v1/module", person);
		must(m.status === 200, `GET /v1/module answered ${m.status}`);
		manifest = m.body;
		must((manifest.features ?? []).includes("ontology"), "features doesn't list ontology");
	});
	await check("descriptor answers people and agents, not anonymous callers", async () => {
		const d = await call("GET", "/v1/ontology", person);
		must(d.status === 200, `a person's token answered ${d.status}`);
		descriptor = d.body;
		must(descriptor.module === manifest?.id, `module ${descriptor.module} isn't the manifest's id ${manifest?.id}`);
		must((await call("GET", "/v1/ontology", agent)).status === 200, "a delegated token can't read the descriptor");
		const none = await call("GET", "/v1/ontology");
		must(none.status === 401 && typeof none.body?.error === "string", `no token answered ${none.status} ${none.text}`);
	});
	await check("descriptor meets the schema and the contract's rules", async () => {
		const errors = checkDescriptor(descriptor);
		must(errors.length === 0, errors.join("; "));
	});
	await check("a module with an undoable action names it in the config", async () => {
		const undoable = Object.entries(descriptor?.action_types ?? {}).filter(([, a]: [string, Json]) => a.agent === "undoable").map(([k]) => k);
		must(undoable.length === 0 || config.undo !== undefined, `the descriptor has ${undoable.join(", ")}, so the config needs undo`);
	});
	await check("descriptor's events are in the manifest", async () => {
		const named = new Set<string>();
		for (const field of ["events", "trigger_events"]) {
			const v = manifest?.[field];
			for (const k of Array.isArray(v) ? v : Object.keys(v ?? {})) named.add(k);
		}
		for (const kind of Object.keys(descriptor?.events ?? {})) must(named.has(kind), `the manifest doesn't name ${kind}`);
	});
	await check("objects have the contract's shape and actions lists", async () => {
		const o = await call("GET", objectPath(config.visible), person);
		must(o.status === 200, `GET ${config.visible} answered ${o.status}`);
		for (const k of ["ref", "type", "title", "revision", "properties", "actions"]) must(k in o.body, `the answer has no ${k}`);
		must(o.body.ref === config.visible, `ref ${o.body.ref}`);
		const type = split(config.visible).type;
		const declared = Object.entries(descriptor.action_types).filter(([, a]: [string, Json]) => a.on === type).map(([k]) => k).sort();
		const listed = o.body.actions.map((a: Json) => a.action).sort();
		must(JSON.stringify(declared) === JSON.stringify(listed), `actions lists ${listed}, the descriptor declares ${declared}`);
		for (const a of o.body.actions) must(typeof a.allowed === "boolean" && (a.allowed || typeof a.reason === "string"), `${a.action} has no reason`);
	});
	await check("objects a caller can't reach answer 404, as missing ones do", async () => {
		const hidden = await call("GET", objectPath(config.visible), outsider);
		const missing = await call("GET", `/v1/objects/${split(config.visible).type}/conformance-missing-${randomUUID()}`, outsider);
		must(hidden.status === 404, `out_of_scope got ${hidden.status}`);
		must(hidden.text === missing.text, `hidden ${hidden.text} and missing ${missing.text} differ`);
	});
	await check("unknown types answer 404 unknown_type", async () => {
		const a = await call("GET", "/v1/objects/conformance_nothing/x", person);
		must(a.status === 404 && a.body?.error === "unknown_type", `answered ${a.status} ${a.text}`);
	});
	await check("delegated tokens are refused on object routes", async () => {
		for (const [method, path, b] of [
			["GET", objectPath(config.visible)],
			["POST", `/v1/objects/${split(config.visible).type}/search`, {}],
			["POST", "/v1/objects/resolve", { refs: [config.visible] }],
		] as const) {
			const a = await call(method, path, agent, b);
			must(a.status === 401, `${method} ${path} answered ${a.status}`);
		}
	});
	await check("search applies scope before sorting and paging", async () => {
		const { type, body: q, limit, expect } = config.search;
		let cursor: string | undefined;
		const seen: string[] = [];
		let first = true;
		for (let pages = 0; pages < 50; pages++) {
			const a = await call("POST", `/v1/objects/${type}/search`, person, { ...q, limit, ...(cursor ? { cursor } : {}) });
			must(a.status === 200, `search answered ${a.status} ${a.text}`);
			const objs = a.body.objects as Json[];
			if (first) must(objs.length === Math.min(limit, expect), `the first page has ${objs.length} objects, not ${Math.min(limit, expect)}`);
			first = false;
			for (const o of objs) {
				must((await call("GET", objectPath(o.ref), person)).status === 200, `search answered ${o.ref}, which get doesn't`);
				seen.push(o.ref);
			}
			cursor = a.body.next_cursor;
			if (!cursor) break;
		}
		must(seen.length === expect && new Set(seen).size === expect, `found ${seen.length} (${new Set(seen).size} distinct), not ${expect}`);
	});
	await check("search refuses bad bodies", async () => {
		const type = config.search.type;
		for (const [what, b] of [
			["a limit over 100", { limit: 101 }],
			["an unknown property", { filter: { property: "conformance_nothing", op: "eq", value: 1 } }],
			["a cursor it didn't issue", { cursor: "Y29uZm9ybWFuY2U" }],
		] as const) {
			const a = await call("POST", `/v1/objects/${type}/search`, person, b);
			must(a.status === 400 && typeof a.body?.error === "string", `${what} answered ${a.status}`);
		}
	});
	await check("resolve leaves out what it can't show", async () => {
		const refs = [config.visible, `${split(config.visible).module}:${split(config.visible).type}:conformance-missing`, "conformance:thing:x"];
		const mine = await call("POST", "/v1/objects/resolve", person, { refs });
		must(mine.status === 200, `answered ${mine.status}`);
		const got = mine.body.objects.map((o: Json) => o.ref);
		must(JSON.stringify(got) === JSON.stringify([config.visible]), `in_scope resolved ${got}`);
		const theirs = await call("POST", "/v1/objects/resolve", outsider, { refs });
		const leaked = theirs.body?.objects?.map((o: Json) => o.ref) ?? [];
		must(theirs.status === 200 && leaked.length === 0, `out_of_scope resolved ${leaked}`);
	});
	await check("delegated tokens are refused on action, proposal and undo routes", async () => {
		for (const [method, path] of [
			["POST", `/v1/actions/${config.apply.action}/validate`],
			["POST", `/v1/actions/${config.apply.action}/apply`],
			["POST", "/v1/undo/conformance"],
			["GET", "/v1/proposals"],
			["POST", "/v1/proposals/conformance/confirm"],
			["POST", "/v1/proposals/conformance/dismiss"],
		] as const) {
			const a = await call(method, path, agent, method === "POST" ? {} : undefined);
			must(a.status === 401, `${method} ${path} answered ${a.status}`);
		}
	});
	await check("apply answers 403 to a visible object it refuses, and 404 to a hidden one", async () => {
		const a = await apply(config.forbidden);
		must(a.status === 403, `answered ${a.status} ${a.text}`);
		const b = await apply(config.apply, outsider);
		must(b.status === 404, `out_of_scope's apply answered ${b.status}`);
	});
	await check("apply answers 409 wrong_state", async () => {
		const a = await apply(config.wrong_state);
		must(a.status === 409 && a.body?.error === "wrong_state", `answered ${a.status} ${a.text}`);
	});
	await check("apply answers 409 stale_revision", async () => {
		const b = await body(config.apply);
		const first = await call("POST", `/v1/actions/${config.apply.action}/apply`, person, b);
		must(first.status === 200, `the first apply answered ${first.status} ${first.text}`);
		const again = await call("POST", `/v1/actions/${config.apply.action}/apply`, person, b);
		must(again.status === 409 && again.body?.error === "stale_revision", `the stale apply answered ${again.status} ${again.text}`);
	});
	await check("idempotent actions replay the first answer, and refuse a new body under the same key", async () => {
		const idempotent = config.idempotent!;
		const key = `conformance-${randomUUID()}`;
		const b = await body(idempotent);
		const path = `/v1/actions/${idempotent.action}/apply`;
		const none = await call("POST", path, person, b);
		must(none.status === 400, `no key answered ${none.status}`);
		const one = await call("POST", path, person, b, { "idempotency-key": key });
		const two = await call("POST", path, person, b, { "idempotency-key": key });
		must(one.status === 200 && two.status === 200, `answered ${one.status} and ${two.status}`);
		must(JSON.stringify(one.body) === JSON.stringify(two.body), `the replay answered ${two.text}, not ${one.text}`);
		const other = await call("POST", path, person, { ...b, revision: (b.revision ?? 0) + 1000 }, { "idempotency-key": key });
		must(other.status === 409, `another body under the same key answered ${other.status}`);
	}, config.idempotent ? undefined : "the config names no idempotent action");
	await check("undo works by ID within its window, once, and only for its person, and lists it until then", async () => {
		const a = await apply(config.undo!, person, `conformance-${randomUUID()}`);
		must(a.status === 200 && typeof a.body?.undo?.id === "string", `apply answered ${a.status} ${a.text}`);
		must(Date.parse(a.body.undo.expires_at) > Date.now(), "expires_at is past");
		const id = a.body.undo.id;
		const listed = async (token: string) => {
			const l = await call("GET", "/v1/undo", token);
			must(l.status === 200 && Array.isArray(l.body?.undo), `GET /v1/undo answered ${l.status} ${l.text}`);
			return (l.body.undo as Json[]).some((u) => u.id === id);
		};
		must(await listed(person), "GET /v1/undo doesn't list what the person can undo");
		must(!(await listed(outsider)), "GET /v1/undo lists another person's undo");
		must((await call("POST", `/v1/undo/${id}`, outsider, {})).status === 404, "another person could undo it");
		must((await call("POST", `/v1/undo/${id}`, agent, {})).status === 401, "a delegated token could undo it");
		const done = await call("POST", `/v1/undo/${id}`, person, {});
		must(done.status === 200, `undo answered ${done.status} ${done.text}`);
		const again = await call("POST", `/v1/undo/${id}`, person, {});
		must(again.status === 409, `a second undo answered ${again.status}`);
		must(!(await listed(person)), "GET /v1/undo still lists an undone action");
	}, config.undo ? undefined : "the config names no undoable action");
	await check("undo after a dependent change answers 409 undo_conflict", async () => {
		const a = await apply(config.undo!, person, `conformance-${randomUUID()}`);
		must(a.status === 200, `apply answered ${a.status} ${a.text}`);
		const dep = await apply(config.undo!.dependent!);
		must(dep.status === 200, `the dependent change answered ${dep.status} ${dep.text}`);
		const u = await call("POST", `/v1/undo/${a.body.undo.id}`, person, {});
		must(u.status === 409 && u.body?.error === "undo_conflict", `undo answered ${u.status} ${u.text}`);
	}, !config.undo ? "the config names no undoable action" : !config.undo.dependent ? "the config names no change that depends on an undoable one" : undefined);
	await check("proposals list the caller's own drafts", async () => {
		const a = await call("GET", "/v1/proposals", person);
		must(a.status === 200 && Array.isArray(a.body?.proposals), `answered ${a.status} ${a.text}`);
	});

	// The generated tools, over MCP, as an agent calls them.
	const mcpPath = config.mcp ?? "/mcp";
	const prefix: string = manifest?.action_prefix ?? descriptor?.module;
	let rpcID = 0;
	type ToolAnswer = { status: number; error?: string; data?: Json; text: string };
	const listTools = async (token?: string): Promise<Json[]> => {
		const a = await call("POST", mcpPath, token, { jsonrpc: "2.0", id: ++rpcID, method: "tools/list" });
		must(a.status === 200 && Array.isArray(a.body?.result?.tools), `tools/list answered ${a.status} ${a.text.slice(0, 200)}`);
		return a.body.result.tools;
	};
	// A tool's answer: its data from inside <module_record>, or its error code (the HTTP one for a refused call).
	const tool = async (name: string, args: Json, token = agent): Promise<ToolAnswer> => {
		const a = await call("POST", mcpPath, token, { jsonrpc: "2.0", id: ++rpcID, method: "tools/call", params: { name, arguments: args } });
		if (a.status !== 200) return { status: a.status, error: a.body?.error ?? String(a.status), text: a.text };
		const result = a.body?.result;
		const text: string = result?.content?.[0]?.text ?? "";
		if (result?.isError) return { status: 200, error: text.split(/[:\s]/)[0], text };
		const m = text.match(/^<module_record module="([a-z][a-z0-9_]*)">\n([\s\S]*)\n<\/module_record>$/);
		if (!m) return { status: 200, text };
		let data: Json;
		try {
			data = JSON.parse(m[2]);
		} catch {
			return { status: 200, text };
		}
		return { status: 200, data, text };
	};
	const toolData = async (name: string, args: Json, token = agent): Promise<Json> => {
		const a = await tool(name, args, token);
		must(a.data !== undefined, `${name} answered ${a.error ? `error ${a.error}` : `no data: ${a.text.slice(0, 200)}`}`);
		return a.data;
	};
	const sensitiveOf = (type: string): string[] => {
		const props = { ...(descriptor?.object_types?.[type]?.properties ?? {}), ...(descriptor?.tenant?.object_types?.[type]?.properties ?? {}) };
		return Object.entries(props).filter(([, p]: [string, Json]) => p?.sensitivity).map(([k]) => k);
	};
	// An object in_scope sees with a value in a sensitive property, as in_scope reads it.
	let sensitiveObject: Json | null | undefined;
	const findSensitive = async (): Promise<Json | null> => {
		if (sensitiveObject !== undefined) return sensitiveObject;
		const candidates: string[] = [];
		if (config.sensitive) candidates.push(config.sensitive);
		else
			for (const type of Object.keys(descriptor?.object_types ?? {}).filter((t) => sensitiveOf(t).length > 0)) {
				const found = await call("POST", `/v1/objects/${encodeURIComponent(type)}/search`, person, { limit: 20 });
				if (found.status === 200) candidates.push(...found.body.objects.map((o: Json) => o.ref));
			}
		sensitiveObject = null;
		for (const ref of candidates) {
			const got = await call("GET", objectPath(ref), person);
			must(got.status === 200, `${ref} isn't visible to in_scope (${got.status})`);
			const props = sensitiveOf(got.body.type).filter((p) => got.body.properties?.[p] != null);
			if (props.length > 0) {
				sensitiveObject = { ...got.body, sensitive: props };
				break;
			}
		}
		return sensitiveObject;
	};
	const declaresSensitive = () => Object.keys(descriptor?.object_types ?? {}).some((t) => sensitiveOf(t).length > 0);
	const generatedNames = () => {
		const names = ["describe", "search", "get", "propose"].map((t) => `${prefix}_${t}`);
		if (Object.values(descriptor?.action_types ?? {}).some((a: Json) => a.agent === "undoable")) names.push(`${prefix}_act`);
		return names;
	};

	await check("tools are generated and listed the same with no token and an agent's", async () => {
		const anonymous = await listTools();
		const names = anonymous.map((t) => t.name);
		for (const name of generatedNames()) must(names.includes(name), `tools/list has no ${name}`);
		if (!generatedNames().includes(`${prefix}_act`)) must(!names.includes(`${prefix}_act`), `${prefix}_act is listed with no undoable action`);
		const withToken = await listTools(agent);
		must(JSON.stringify(withToken) === JSON.stringify(anonymous), "the list with an agent's token differs from the one with no token");
	});

	await check("tool results are marked as data", async () => {
		const a = await tool(`${prefix}_get`, { ref: config.visible });
		must(a.data !== undefined && a.data.ref === config.visible, `${prefix}_get answered outside <module_record>: ${a.text.slice(0, 200)}`);
	});

	await check("describe leaves out the actions agents may not take", async () => {
		const d = await toolData(`${prefix}_describe`, {});
		for (const [name, a] of Object.entries(d.action_types ?? {}) as [string, Json][]) must(a.agent !== "never", `it lists ${name}, whose tier is never`);
		for (const name of descriptor.restricted ?? []) must(!(name in (d.action_types ?? {})), `it lists ${name}, which is restricted`);
	});

	const never = Object.entries(descriptor?.action_types ?? {}).find(([, a]: [string, Json]) => a.agent === "never");
	await check(
		"agents can't draft an action whose tier is never",
		async () => {
			const [name, a] = never as [string, Json];
			let object: string | undefined;
			if (a.on) {
				const found = await call("POST", `/v1/objects/${encodeURIComponent(a.on)}/search`, person, { limit: 1 });
				object = found.status === 200 ? found.body.objects[0]?.ref : undefined;
			}
			const before = await call("GET", "/v1/proposals", person);
			const got = await tool(`${prefix}_propose`, { action: name, ...(object ? { object } : {}), params: {} });
			must(got.error === "not_allowed_for_agents", `${prefix}_propose of ${name} answered ${got.error ?? "a draft"}`);
			const after = await call("GET", "/v1/proposals", person);
			must(after.body?.proposals?.length === before.body?.proposals?.length, "a draft was stored");
		},
		never ? undefined : "the descriptor declares no action with tier never",
	);

	await check(
		"agents get every sensitive value masked",
		async () => {
			const o = await findSensitive();
			must(o, "no object in_scope sees has a value in a sensitive property: name one as `sensitive` in the config");
			const seen = await toolData(`${prefix}_get`, { ref: o.ref });
			for (const p of o.sensitive) {
				const v = seen.properties?.[p];
				must(v == null || JSON.stringify(v) !== JSON.stringify(o.properties[p]), `${prefix}_get answers ${o.type}.${p} unmasked`);
			}
			const titleProp = descriptor.object_types[o.type]?.title;
			if (o.sensitive.includes(titleProp)) must(seen.title !== o.title, `${prefix}_get answers ${o.type}'s title, ${titleProp}, unmasked`);
		},
		declaresSensitive() ? undefined : "the descriptor declares no sensitive property",
	);

	await check(
		"agents can't filter or sort on a sensitive property",
		async () => {
			const o = await findSensitive();
			const type: string = o?.type ?? Object.keys(descriptor.object_types).find((t) => sensitiveOf(t).length > 0);
			const p: string = o?.sensitive[0] ?? sensitiveOf(type)[0];
			const value = o?.properties?.[p] ?? "x";
			const filtered = await tool(`${prefix}_search`, { type, filter: { property: p, op: "eq", value } });
			must(filtered.error === "filter_not_allowed", `a filter on ${type}.${p} answered ${filtered.error ?? "results"}`);
			const sorted = await tool(`${prefix}_search`, { type, sort: { property: p } });
			must(sorted.error === "filter_not_allowed", `a sort on ${type}.${p} answered ${sorted.error ?? "results"}`);
		},
		declaresSensitive() ? undefined : "the descriptor declares no sensitive property",
	);

	await check(
		"each tool needs its grant action",
		async () => {
			const readOnly = config.tokens.agent_read_only as string;
			await toolData(`${prefix}_get`, { ref: config.visible }, readOnly);
			const before = await call("GET", "/v1/proposals", person);
			const got = await tool(`${prefix}_propose`, { action: config.apply.action, object: await resolveRef(config.apply.object), params: (await body(config.apply)).params }, readOnly);
			must(got.error === "forbidden" || got.status === 403, `${prefix}_propose with only ${prefix}:read answered ${got.error ?? "a draft"}`);
			const after = await call("GET", "/v1/proposals", person);
			must(after.body?.proposals?.length === before.body?.proposals?.length, "a draft was stored");
		},
		config.tokens.agent_read_only ? undefined : "the config names no tokens.agent_read_only",
	);
	return failed;
}

const here = fileURLToPath(new URL(".", import.meta.url));

// The check each of the stub's -break modes must fail.
const breaks: Record<string, string> = {
	scope: "search applies scope",
	paging: "search applies scope",
	hidden: "objects a caller can't reach",
	"unknown-type": "unknown types",
	resolve: "resolve leaves out",
	delegated: "delegated tokens are refused on object routes",
	forbidden: "apply answers 403",
	"wrong-state": "apply answers 409 wrong_state",
	stale: "apply answers 409 stale_revision",
	replay: "idempotent actions replay",
	"undo-owner": "undo works by ID",
	"undo-list": "undo works by ID",
	"undo-conflict": "undo after a dependent change",
	tier: "descriptor meets the schema",
	undoable: "descriptor meets the schema",
	"tenant-action": "descriptor meets the schema",
	label: "descriptor meets the schema",
	events: "descriptor's events",
	interface: "descriptor meets the schema",
	"tools-tenant": "tools are generated and listed the same",
	"agent-never": "describe leaves out the actions agents may not take",
	"agent-mask": "agents get every sensitive value masked",
	"agent-filter": "agents can't filter or sort on a sensitive property",
	"agent-grant": "each tool needs its grant action",
	"agent-data": "tool results are marked as data",
};

async function freePort(): Promise<number> {
	return new Promise((resolve) => {
		const s = createServer().listen(0, "127.0.0.1", () => {
			const port = (s.address() as { port: number }).port;
			s.close(() => resolve(port));
		});
	});
}

async function withStub<T>(binary: string, rule: string, run: (base: string) => Promise<T>): Promise<T> {
	const port = await freePort();
	const child: ChildProcess = spawn(binary, ["-addr", `127.0.0.1:${port}`, ...(rule ? ["-break", rule] : [])], { stdio: "ignore" });
	const base = `http://127.0.0.1:${port}`;
	try {
		for (let i = 0; ; i++) {
			try {
				await fetch(`${base}/v1/module`);
				break;
			} catch {
				if (i > 100) throw new Error("the stub didn't start");
				await new Promise((r) => setTimeout(r, 50));
			}
		}
		return await run(base);
	} finally {
		child.kill();
	}
}

async function selfTest(): Promise<number> {
	const modkit = join(here, "../../t10-modkit");
	const dir = mkdtempSync(join(tmpdir(), "ontology-stub-"));
	const binary = join(dir, "stub");
	try {
		execFileSync("go", ["build", "-o", binary, "./ontology/testdata/ledgermod/stub"], { cwd: modkit, stdio: "inherit" });
		const config = JSON.parse(readFileSync(join(here, "testdata/stub.json"), "utf8")) as Config;
		let bad = 0;
		const clean = await withStub(binary, "", (base) => runSuite(base, config));
		if (clean.length > 0) {
			bad++;
			console.error(`the unbroken stub fails:\n  ${clean.join("\n  ")}`);
		}
		for (const [rule, want] of Object.entries(breaks)) {
			const failed = await withStub(binary, rule, (base) => runSuite(base, config));
			if (!failed.some((f) => f.startsWith(want))) {
				bad++;
				console.error(`-break ${rule} should fail "${want}", but failed: ${failed.join("; ") || "nothing"}`);
			}
		}
		console.log(bad === 0 ? `ontology conformance self-test passed (${Object.keys(breaks).length} broken stubs caught)` : `ontology conformance self-test: ${bad} wrong`);
		return bad === 0 ? 0 : 1;
	} finally {
		rmSync(dir, { recursive: true, force: true });
	}
}

if (process.argv[1] !== undefined && fileURLToPath(import.meta.url) === process.argv[1]) {
	const args = process.argv.slice(2);
	if (args[0] === "--self-test") process.exit(await selfTest());
	if (args.length !== 2) {
		console.error("usage: conformance.ts <module URL> <config.json> | --self-test");
		process.exit(2);
	}
	const failed = await runSuite(args[0].replace(/\/+$/, ""), JSON.parse(readFileSync(args[1], "utf8")), console.log);
	console.log(failed.length === 0 ? "ontology conformance passed" : `ontology conformance: ${failed.length} failed`);
	process.exit(failed.length === 0 ? 0 : 1);
}
