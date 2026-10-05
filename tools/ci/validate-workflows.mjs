/**
 * .github/workflows 的结构校验（与 Armadra 同一套作业关系检查，另加本仓三条硬规则）。
 *
 *   node tools/ci/validate-workflows.mjs
 *
 * 作业关系（needs 指向不存在的作业、成环、读了没声明的 needs、steps 输出的 id 拼错、
 * runner 标签不存在）只有在工作流真的跑起来时才暴露；这里在 PR 上就拦下。本仓另加：
 *
 * - 每个外部 action 钉到 40 位提交 SHA（标签可以被改写，SHA 不能）；
 * - 工作流顶层必须声明 `permissions`，且不得是 `write-all`；
 * - 每个作业必须写 `timeout-minutes`。
 */
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { parseYaml } from "./workflow-yaml.mjs";

const root = fileURLToPath(new URL("../../", import.meta.url));
const WORKFLOW_DIR = join(root, ".github/workflows");

export const RUNNERS = new Set([
  "ubuntu-latest",
  "ubuntu-24.04",
  "ubuntu-22.04",
  "ubuntu-24.04-arm",
  "ubuntu-22.04-arm",
]);

function matrixValues(job, key) {
  const matrix = job?.strategy?.matrix;
  if (!matrix || typeof matrix !== "object") return [];
  const values = [];
  const direct = matrix[key];
  if (Array.isArray(direct)) values.push(...direct);
  if (Array.isArray(matrix.include)) {
    for (const entry of matrix.include) {
      if (entry && typeof entry === "object" && entry[key] !== undefined)
        values.push(entry[key]);
    }
  }
  return values.map(String);
}

function runnerLabels(job) {
  const runsOn = job["runs-on"];
  if (typeof runsOn !== "string") return [];
  const expression = runsOn.match(/^\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}$/);
  if (expression) return matrixValues(job, expression[1]);
  return /\$\{\{/.test(runsOn) ? [] : [runsOn];
}

function neededJobs(value, found = new Set()) {
  if (typeof value === "string") {
    for (const match of value.matchAll(/needs\.([A-Za-z0-9_-]+)/g))
      found.add(match[1]);
    return found;
  }
  if (Array.isArray(value)) {
    for (const item of value) neededJobs(item, found);
    return found;
  }
  if (value && typeof value === "object") {
    for (const item of Object.values(value)) neededJobs(item, found);
  }
  return found;
}

/** `owner/repo@<40 位 SHA>`；本地 action（`./`）与 `docker://` 不在此列。 */
export function isPinnedAction(uses) {
  if (uses.startsWith("./") || uses.startsWith("docker://")) return true;
  return /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_./-]+@[0-9a-f]{40}$/.test(uses);
}

export function checkWorkflow(name, document) {
  const problems = [];
  const say = (message) => problems.push(`${name}: ${message}`);
  if (!document || typeof document !== "object") {
    say("is not a mapping");
    return problems;
  }
  if (typeof document.name !== "string" || document.name === "")
    say("has no name");
  const triggers = document.on ?? document[true];
  if (!triggers) say("has no trigger");
  if (document.permissions === undefined)
    say("declares no top-level permissions");
  else if (document.permissions === "write-all")
    say("grants write-all permissions");
  const jobs = document.jobs;
  if (!jobs || typeof jobs !== "object" || Array.isArray(jobs)) {
    say("has no jobs");
    return problems;
  }
  const names = new Set(Object.keys(jobs));
  if (names.size === 0) say("has no jobs");

  for (const [jobName, job] of Object.entries(jobs)) {
    const where = `${name}: job ${jobName}`;
    if (!job || typeof job !== "object" || Array.isArray(job)) {
      problems.push(`${where} is not a mapping`);
      continue;
    }
    if (!job["runs-on"] && !job.uses)
      problems.push(`${where} names no runner and reuses no workflow`);
    if (!job.uses && job["timeout-minutes"] === undefined)
      problems.push(`${where} has no timeout-minutes`);
    if (job.permissions === "write-all")
      problems.push(`${where} grants write-all permissions`);
    for (const label of runnerLabels(job)) {
      if (!RUNNERS.has(label))
        problems.push(
          `${where} runs on ${label}, which is not an allowed runner`,
        );
    }
    const declared = new Set(
      job.needs === undefined
        ? []
        : Array.isArray(job.needs)
          ? job.needs
          : [job.needs],
    );
    for (const dependency of declared) {
      if (!names.has(dependency))
        problems.push(
          `${where} needs ${dependency}, which is not a job in this workflow`,
        );
      if (dependency === jobName) problems.push(`${where} needs itself`);
    }
    for (const referenced of neededJobs({ ...job, needs: undefined })) {
      if (!declared.has(referenced))
        problems.push(
          `${where} reads needs.${referenced} without declaring it in needs`,
        );
    }
    if (job.uses && !isPinnedAction(String(job.uses)))
      problems.push(`${where} reuses ${job.uses} without pinning a commit SHA`);
    const steps = job.steps;
    if (steps === undefined) {
      if (!job.uses) problems.push(`${where} has no steps`);
      continue;
    }
    if (!Array.isArray(steps) || steps.length === 0) {
      problems.push(`${where} has no steps`);
      continue;
    }
    const stepIds = new Set();
    for (const [index, step] of steps.entries()) {
      const stepWhere = `${where} step ${index + 1}`;
      if (!step || typeof step !== "object" || Array.isArray(step)) {
        problems.push(`${stepWhere} is not a mapping`);
        continue;
      }
      if (!step.run && !step.uses)
        problems.push(`${stepWhere} neither runs nor uses anything`);
      if (step.run && step.uses)
        problems.push(`${stepWhere} both runs and uses something`);
      if (step.uses && !isPinnedAction(String(step.uses)))
        problems.push(
          `${stepWhere} uses ${step.uses} without pinning a commit SHA`,
        );
      if (step.id) {
        if (stepIds.has(step.id))
          problems.push(`${stepWhere} repeats the id ${step.id}`);
        stepIds.add(step.id);
      }
    }
    for (const match of JSON.stringify(job).matchAll(
      /steps\.([A-Za-z0-9_-]+)\.outputs/g,
    )) {
      if (!stepIds.has(match[1]))
        problems.push(
          `${where} reads steps.${match[1]}.outputs, but no step has that id`,
        );
    }
  }

  const state = new Map();
  const visit = (jobName, trail) => {
    if (state.get(jobName) === "done") return;
    if (state.get(jobName) === "open") {
      say(`jobs form a cycle: ${[...trail, jobName].join(" -> ")}`);
      return;
    }
    state.set(jobName, "open");
    const job = jobs[jobName];
    const needs =
      job?.needs === undefined
        ? []
        : Array.isArray(job.needs)
          ? job.needs
          : [job.needs];
    for (const dependency of needs) {
      if (names.has(dependency)) visit(dependency, [...trail, jobName]);
    }
    state.set(jobName, "done");
  };
  for (const jobName of names) visit(jobName, []);
  return problems;
}

export function checkWorkflowDirectory(directory = WORKFLOW_DIR) {
  const problems = [];
  const files = readdirSync(directory).filter((name) => /\.ya?ml$/.test(name));
  if (files.length === 0) problems.push(`${directory} holds no workflows`);
  for (const file of files.sort()) {
    let document;
    try {
      document = parseYaml(readFileSync(join(directory, file), "utf8"));
    } catch (error) {
      problems.push(`${file}: ${error.message}`);
      continue;
    }
    problems.push(...checkWorkflow(file, document));
  }
  return { files, problems };
}

function main() {
  const { files, problems } = checkWorkflowDirectory();
  for (const problem of problems) console.error(`✗ ${problem}`);
  if (problems.length > 0) {
    console.error(
      `\nWorkflow validation failed: ${problems.length} problem(s)`,
    );
    return 1;
  }
  console.log(`${files.length} workflow(s) validated: ${files.join(", ")}`);
  return 0;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exit(main());
}
