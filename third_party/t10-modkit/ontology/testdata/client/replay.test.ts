/**
 * Holds web/ontology.ts to the recorded answers the Go client is held to (fixture.json, recorded by
 * ontology/client's tests with ONTOLOGY_RECORD=1), so the two clients give the same results.
 *
 *   node --test ontology/testdata/client/replay.test.ts
 *
 * The cases with a service's or a delegated token are the Go client's alone: a browser holds no token.
 */
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { ApiError } from "../../../web/api.ts";
import { allowedActions, formatValue, OntologyClient, paramsOf, propertiesOf, splitRef, workbenchModules } from "../../../web/ontology.ts";

type Exchange = { module: string; method: string; path: string; body?: string; status: number; response: unknown };
type Case = { name: string; op: string; token: string; args: Record<string, any>; result?: unknown; error?: { status: number; code: string } };
const fixture = JSON.parse(readFileSync(new URL("fixture.json", import.meta.url), "utf8")) as {
	modules: { id: string; down?: boolean }[];
	exchanges: Exchange[];
	cases: Case[];
};

// A body with sorted keys, as the Go side records it.
function canonical(text: string): string {
	if (text.trim() === "") return "";
	const sort = (v: unknown): unknown =>
		Array.isArray(v) ? v.map(sort) : v !== null && typeof v === "object" ? Object.fromEntries(Object.keys(v).sort().map((k) => [k, sort((v as Record<string, unknown>)[k])])) : v;
	return JSON.stringify(sort(JSON.parse(text)));
}

function replay(): typeof fetch {
	const used = new Set<number>();
	return (async (input: string | URL | Request, init?: RequestInit) => {
		const url = String(input);
		if (url.startsWith("http://down")) throw new TypeError("fetch failed");
		const rest = url.replace(/^\/m\//, "");
		const slash = rest.indexOf("/");
		const module = rest.slice(0, slash);
		const path = rest.slice(slash);
		const method = init?.method ?? "GET";
		const body = canonical(typeof init?.body === "string" ? init.body : "");
		const i = fixture.exchanges.findIndex((e, n) => !used.has(n) && e.module === module && e.method === method && e.path === path && (e.body ?? "") === body);
		if (i < 0) throw new Error(`no recorded answer to ${module} ${method} ${path} ${body}`);
		used.add(i);
		const e = fixture.exchanges[i];
		return new Response(JSON.stringify(e.response), { status: e.status, headers: { "content-type": "application/json" } });
	}) as typeof fetch;
}

async function run(c: OntologyClient, op: string, a: Record<string, any>): Promise<unknown> {
	switch (op) {
		case "load": {
			const loaded = await c.load();
			return { ontologies: Object.keys(loaded.descriptors).sort(), unavailable: loaded.unavailable };
		}
		case "describe":
			return c.describe(a.module);
		case "search":
			return c.search(a.module, a.type, a.query);
		case "get":
			return c.get(a.ref, a.sources);
		case "links":
			return c.links(a.ref, a.link, a.limit, a.cursor);
		case "resolve":
			return c.resolve(a.refs);
		case "validate":
			return c.validate(a.module, a.action, a.request);
		case "apply":
			return c.apply(a.module, a.action, a.request, a.key);
		case "undo":
			return c.undo(a.module, a.id);
		case "proposals":
			return { proposals: await c.proposals(a.module) };
		case "confirm":
			return c.confirm(a.module, a.id, a.body);
		case "dismiss":
			await c.dismiss(a.module, a.id);
			return {};
	}
	throw new Error(`unknown op ${op}`);
}

test("the TypeScript client gives the Go client's results", async () => {
	const modules = fixture.modules.map((m) => ({ id: m.id, url: m.down ? "http://down" : `/m/${m.id}` }));
	const c = new OntologyClient({ modules, fetch: replay() });
	for (const tc of fixture.cases) {
		if (tc.token !== "person") continue;
		if (tc.error) {
			await assert.rejects(run(c, tc.op, tc.args), (e: unknown) => e instanceof ApiError && e.status === tc.error!.status && e.code === tc.error!.code, tc.name);
			continue;
		}
		assert.deepEqual(JSON.parse(JSON.stringify(await run(c, tc.op, tc.args))), tc.result, tc.name);
	}
});

test("the helpers the components draw with", () => {
	const describe = fixture.cases.find((c) => c.name === "describe")!.result as any;
	assert.deepEqual(propertiesOf(describe, "deal").stage.values, ["线索", "商机", "在跟"]);
	assert.equal(propertiesOf(describe, "stakeholder").F.sensitivity, "personal");
	assert.deepEqual(paramsOf(describe, "record.add").kind.values, ["拜访"]);
	const deal = fixture.cases.find((c) => c.name === "get a deal")!.result as any;
	assert.ok(allowedActions(deal).includes("record.add"));
	assert.ok(!allowedActions(deal).includes("deal.set_discount"));
	assert.equal(formatValue({ type: "money_fen" }, 12000000, "en"), "CN¥120,000.00");
	assert.deepEqual(splitRef("sales:deal:deal_0001"), { module: "sales", type: "deal", id: "deal_0001" });
	assert.equal(splitRef("not-a-ref"), undefined);
	// In a browser, calls go through Workbench's proxy, with no token in the page.
	assert.deepEqual(workbenchModules(["property"]), [{ id: "property", url: "/api/modules/property" }]);
});

test("a call in a browser goes through the proxy", async () => {
	const seen: string[] = [];
	const fake = (async (input: string | URL | Request, init?: RequestInit) => {
		seen.push(`${init?.method} ${input} ${JSON.stringify(init?.headers)}`);
		if (String(input).endsWith("/v1/module")) return new Response(JSON.stringify({ api_version: 1, features: ["ontology"] }));
		if (String(input).endsWith("/v1/ontology")) return new Response(JSON.stringify({ module: "property" }));
		return new Response(JSON.stringify({ objects: [] }));
	}) as typeof fetch;
	const c = new OntologyClient({ modules: workbenchModules(["property"]), fetch: fake });
	await c.load();
	await c.search("property", "work_order", { limit: 1 });
	assert.ok(seen.at(-1)!.startsWith("POST /api/modules/property/v1/objects/work_order/search"));
	assert.ok(!seen.some((s) => /authorization/i.test(s)));
});
