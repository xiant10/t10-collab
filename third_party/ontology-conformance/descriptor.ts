/**
 * Checks an ontology descriptor: against descriptor.schema.json, then against the contract's rules that cross-reference
 * parts of a descriptor, which a schema can't hold. The conformance suite and t10-template's checks use it too.
 *
 *   node contracts/ontology/descriptor.ts file.json...   # check descriptors; exits 1 on any error
 *   node contracts/ontology/descriptor.ts --self-test    # the examples in testdata/ give the expected results
 *
 * It needs nothing beyond Node: the schema validator covers the keywords the schema uses.
 */
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const here = fileURLToPath(new URL(".", import.meta.url));
type Json = null | boolean | number | string | Json[] | { [key: string]: Json };
type Obj = { [key: string]: Json };
const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v);
const obj = (v: unknown): Obj => (isObj(v) ? v : {});

export const schema = JSON.parse(readFileSync(join(here, "descriptor.schema.json"), "utf8")) as Obj;
export const interfaces = JSON.parse(readFileSync(join(here, "interfaces.json"), "utf8")) as Record<string, { properties: Record<string, Obj> }>;

// The JSON Schema keywords descriptor.schema.json uses.
function typeOf(v: unknown): string {
	if (v === null) return "null";
	if (Array.isArray(v)) return "array";
	if (typeof v === "number") return Number.isInteger(v) ? "integer" : "number";
	return typeof v;
}
function resolve(s: Obj): Obj {
	const ref = s.$ref;
	if (typeof ref !== "string") return s;
	const name = ref.replace("#/$defs/", "");
	return resolve(obj(obj(schema.$defs)[name]));
}
function validate(value: unknown, raw: Obj, path: string, errors: string[]): void {
	const s = resolve(raw);
	const fail = (msg: string) => errors.push(`${path || "/"}: ${msg}`);
	if (typeof s.type === "string") {
		const t = typeOf(value);
		if (!(t === s.type || (s.type === "number" && t === "integer"))) return fail(`is ${t}, not ${s.type}`);
	}
	if (Array.isArray(s.enum) && !s.enum.includes(value as Json)) return fail(`is not one of ${s.enum.join(", ")}`);
	if ("const" in s && s.const !== value) return fail(`is not ${JSON.stringify(s.const)}`);
	if (typeof value === "string") {
		if (typeof s.pattern === "string" && !new RegExp(s.pattern, "u").test(value)) fail(`"${value}" doesn't match ${s.pattern}`);
		if (typeof s.minLength === "number" && [...value].length < s.minLength) fail("is too short");
		if (typeof s.maxLength === "number" && [...value].length > s.maxLength) fail("is too long");
	}
	if (typeof value === "number" && typeof s.minimum === "number" && value < s.minimum) fail(`is below ${s.minimum}`);
	if (Array.isArray(value) && isObj(s.items)) value.forEach((item, i) => validate(item, s.items as Obj, `${path}/${i}`, errors));
	if (isObj(value)) {
		for (const key of (s.required as string[] | undefined) ?? []) if (!(key in value)) fail(`needs ${key}`);
		const props = obj(s.properties);
		for (const [key, item] of Object.entries(value)) {
			if (isObj(s.propertyNames)) validate(key, s.propertyNames, `${path}/${key} (name)`, errors);
			if (key in props) validate(item, obj(props[key]), `${path}/${key}`, errors);
			else if (s.additionalProperties === false) fail(`has unknown ${key}`);
			else if (isObj(s.additionalProperties)) validate(item, s.additionalProperties, `${path}/${key}`, errors);
		}
	}
	for (const sub of (s.allOf as Obj[] | undefined) ?? []) validate(value, sub, path, errors);
	if (isObj(s.not)) {
		const inner: string[] = [];
		validate(value, s.not, path, inner);
		if (inner.length === 0) fail("has what it mustn't");
	}
	if (isObj(s.if)) {
		const inner: string[] = [];
		validate(value, s.if, path, inner);
		const branch = inner.length === 0 ? s.then : s.else;
		if (isObj(branch)) validate(value, branch, path, errors);
	}
}

/** Every problem with a descriptor, empty when it meets the contract. */
export function checkDescriptor(d: unknown): string[] {
	const errors: string[] = [];
	validate(d, schema, "", errors);
	if (errors.length > 0 || !isObj(d)) return errors;
	const err = (msg: string) => errors.push(msg);
	const module = d.module as string;
	const types = obj(d.object_types);
	const actions = obj(d.action_types);
	const local = (to: string) => to.split(":")[0] === module;
	const declared = (to: string) => !local(to) || to.split(":")[1] in types;

	const used = new Set<string>();
	for (const [name, rawType] of Object.entries(types)) {
		const t = obj(rawType);
		const props = obj(t.properties);
		if (!(String(t.title) in props)) err(`${name}: its title ${t.title} isn't one of its properties`);
		for (const iface of t.implements as string[]) {
			used.add(iface);
			const def = interfaces[iface];
			if (def === undefined) {
				err(`${name}: ${iface} isn't an interface the contract defines`);
				continue;
			}
			for (const [prop, want] of Object.entries(def.properties)) {
				const have = obj(props[prop]);
				if (have.type !== want.type || (want.to !== undefined && have.to !== want.to)) {
					err(`${name}: ${iface} needs ${prop} as ${want.type}${want.to ? ` to ${want.to}` : ""}`);
				}
			}
		}
		for (const [prop, rawProp] of Object.entries(props)) {
			const p = obj(rawProp);
			if (typeof p.to === "string" && !declared(p.to)) err(`${name}.${prop}: ${p.to} isn't a declared type`);
		}
		for (const [link, rawLink] of Object.entries(obj(t.links))) {
			const l = obj(rawLink);
			if (!declared(String(l.to))) err(`${name}.links.${link}: ${l.to} isn't a declared type`);
		}
	}
	const listed = new Set(d.interfaces as string[]);
	for (const iface of used) if (!listed.has(iface)) err(`interfaces: leaves out ${iface}, which a type implements`);
	for (const iface of listed) if (!used.has(iface)) err(`interfaces: lists ${iface}, which no type implements`);

	for (const [name, rawAction] of Object.entries(actions)) {
		const a = obj(rawAction);
		if (a.on !== undefined && !(String(a.on) in types)) {
			err(`${name}: acts on ${a.on}, which isn't a declared type`);
			continue;
		}
		const props = obj(obj(types[String(a.on)]).properties);
		for (const [param, rawParam] of Object.entries(obj(a.params))) {
			const p = obj(rawParam);
			if (obj(props[param]).derived === true) err(`${name}: param ${param} would set the derived ${a.on}.${param}`);
			if (typeof p.to === "string" && !declared(p.to)) err(`${name}.${param}: ${p.to} isn't a declared type`);
		}
	}
	for (const name of d.restricted as string[]) {
		if (!(name in actions)) err(`restricted: ${name} isn't a declared action`);
		else if (obj(actions[name]).agent !== "never") err(`restricted: ${name} has tier ${obj(actions[name]).agent}, not never`);
	}
	for (const [kind, rawEvent] of Object.entries(obj(d.events))) {
		if (!(String(obj(rawEvent).on) in types)) err(`events.${kind}: ${obj(rawEvent).on} isn't a declared type`);
	}
	errors.push(...checkTenant(d));
	return errors;
}

/** The tenant layer only adds: values to enums, and properties under new names. */
function checkTenant(d: Obj): string[] {
	const errors: string[] = [];
	const err = (msg: string) => errors.push(`tenant: ${msg}`);
	const tenant = obj(d.tenant);
	const types = obj(d.object_types);
	const actions = obj(d.action_types);
	const addValues = (where: string, values: Obj, core: Obj, added: Obj) => {
		for (const prop of Object.keys(values)) {
			const p = obj(core[prop] ?? added[prop]);
			if (!(prop in core) && !(prop in added)) err(`${where}.${prop}: adds values to a property that isn't declared`);
			else if (p.type !== "enum") err(`${where}.${prop}: adds values to a ${p.type}, not an enum`);
		}
	};
	const addProperties = (where: string, added: Obj, core: Obj) => {
		for (const prop of Object.keys(added)) if (prop in core) err(`${where}.${prop}: redeclares a core property`);
	};
	for (const [name, rawLayer] of Object.entries(obj(tenant.object_types))) {
		const layer = obj(rawLayer);
		if (!(name in types)) {
			err(`object_types.${name}: isn't a core type; a tenant layer adds no types`);
			continue;
		}
		const t = obj(types[name]);
		const coreProps = obj(t.properties);
		addProperties(name, obj(layer.properties), coreProps);
		addValues(name, obj(layer.values), coreProps, obj(layer.properties));
		for (const [link, rawLinkLayer] of Object.entries(obj(layer.links))) {
			const linkLayer = obj(rawLinkLayer);
			const coreLink = obj(obj(t.links)[link]);
			if (!(link in obj(t.links))) {
				err(`${name}.links.${link}: isn't a core link`);
				continue;
			}
			addProperties(`${name}.links.${link}`, obj(linkLayer.properties), obj(coreLink.properties));
			addValues(`${name}.links.${link}`, obj(linkLayer.values), obj(coreLink.properties), obj(linkLayer.properties));
		}
	}
	for (const [name, rawLayer] of Object.entries(obj(tenant.action_types))) {
		if (!(name in actions)) {
			err(`action_types.${name}: declares an action type; a tenant layer adds none`);
			continue;
		}
		const a = obj(actions[name]);
		const params = obj(a.params);
		for (const param of Object.keys(obj(obj(rawLayer).values))) {
			const p = obj(params[param]);
			if (!(param in params)) err(`${name}.${param}: adds values to a parameter that isn't declared`);
			else if (p.type !== "enum") err(`${name}.${param}: adds values to a ${p.type}, not an enum`);
			else if (p.label !== true && a.agent !== "never") err(`${name}.${param}: adds values to a parameter that isn't "label": true, on a ${a.agent} action`);
		}
	}
	return errors;
}

function selfTest(): number {
	const dir = join(here, "testdata");
	const expect = JSON.parse(readFileSync(join(dir, "invalid", "expect.json"), "utf8")) as Record<string, string>;
	let failed = 0;
	for (const file of readdirSync(join(dir, "valid")).filter((f) => f.endsWith(".json"))) {
		const errors = checkDescriptor(JSON.parse(readFileSync(join(dir, "valid", file), "utf8")));
		if (errors.length > 0) {
			failed++;
			console.error(`valid/${file} fails:\n  ${errors.join("\n  ")}`);
		}
	}
	const files = readdirSync(join(dir, "invalid")).filter((f) => f.endsWith(".json") && f !== "expect.json");
	for (const file of files) {
		const want = expect[file];
		const errors = checkDescriptor(JSON.parse(readFileSync(join(dir, "invalid", file), "utf8")));
		if (want === undefined) {
			failed++;
			console.error(`invalid/${file} has no expected error in expect.json`);
		} else if (!errors.some((e) => e.includes(want))) {
			failed++;
			console.error(`invalid/${file} should fail with "${want}", got:\n  ${errors.join("\n  ") || "(no errors)"}`);
		}
	}
	for (const file of Object.keys(expect)) if (!files.includes(file)) (failed++, console.error(`expect.json names missing ${file}`));
	console.log(failed === 0 ? "descriptor self-test passed" : `descriptor self-test: ${failed} failed`);
	return failed === 0 ? 0 : 1;
}

if (process.argv[1] !== undefined && fileURLToPath(import.meta.url) === process.argv[1]) {
	const args = process.argv.slice(2);
	if (args[0] === "--self-test") process.exit(selfTest());
	if (args.length === 0) {
		console.error("usage: descriptor.ts --self-test | file.json...");
		process.exit(2);
	}
	let bad = 0;
	for (const file of args) {
		const errors = checkDescriptor(JSON.parse(readFileSync(file, "utf8")));
		if (errors.length > 0) (bad++, console.error(`${file}:\n  ${errors.join("\n  ")}`));
	}
	process.exit(bad === 0 ? 0 : 1);
}
