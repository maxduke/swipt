import { execFile } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import { createRequire } from "node:module";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";

import { label, sameName } from "./connection.mjs";
import { tr } from "./i18n.mjs";
import { choose, interactive, step } from "./output.mjs";

const exec = promisify(execFile);

const xcodeMissing = tr(
  "Xcode is missing; install it from the App Store, open it once, and select it with xcode-select",
);

export async function devicectl(args, { parse = false } = {}) {
  const output = parse
    ? path.join(os.tmpdir(), `devicectl-${randomUUID()}.json`)
    : undefined;
  try {
    await exec(
      "xcrun",
      ["devicectl", ...args, ...(parse ? ["--json-output", output] : [])],
      { maxBuffer: Infinity },
    );
  } catch (error) {
    if (error.code === "ENOENT") {
      throw new Error(xcodeMissing);
    }
    if (typeof error.code !== "number") {
      throw error;
    }
    const text = `${error.stdout}${error.stderr}`;
    if (/^(xcrun|xcode-select): /m.test(text)) {
      throw new Error(`${xcodeMissing} (${text.trim()})`);
    }
    if (text.includes("4016")) {
      throw new Error(
        tr(
          "CoreDevice cannot reach the device while its loopback VPN is on; turn off Swipt's VPN on the device, then run this again",
        ),
      );
    }
    throw new Error(text.trim() || `devicectl exited with ${error.code}`);
  }
  if (!parse) {
    return undefined;
  }
  const parsed = JSON.parse(fs.readFileSync(output, "utf8"));
  fs.rmSync(output);
  return parsed;
}

export const serverHint = tr(
  'swipt-server is missing; install it with "npm install -g @swipt/cli --include=optional"',
);

export function serverPath() {
  const built = path.join(
    path.dirname(import.meta.dirname),
    "build/swipt-server",
  );
  if (fs.existsSync(built)) {
    return built;
  }
  let packaged;
  try {
    packaged = path.join(
      path.dirname(
        createRequire(import.meta.url).resolve(
          `@swipt/server-${process.platform}-${process.arch}/package.json`,
        ),
      ),
      "swipt-server",
    );
  } catch (error) {
    if (error.code !== "MODULE_NOT_FOUND") {
      throw error;
    }
    return "swipt-server";
  }
  return fs.existsSync(packaged) ? packaged : "swipt-server";
}

async function guest(device, args) {
  try {
    await exec(serverPath(), [
      "guest",
      "-d",
      device.udid,
      "--socket",
      device.socket,
      ...args,
    ]);
  } catch (error) {
    if (error.code === "ENOENT") {
      throw new Error(serverHint);
    }
    throw new Error(
      `${error.stderr ?? ""}`.replace(/^Error: /, "").trim() || error.message,
    );
  }
}

function callGuest(device, method, params = {}) {
  return new Promise((resolve, reject) => {
    const socket = net.createConnection(device.socket, () =>
      socket.write(`${JSON.stringify({ t: "rpc", method, params })}\n`),
    );
    const chunks = [];
    socket.on("data", (chunk) => {
      chunks.push(chunk);
      if (chunk.includes(10)) {
        socket.destroy();
        try {
          const reply = JSON.parse(Buffer.concat(chunks));
          if (reply.ok) {
            resolve(reply.result);
          } else {
            reject(new Error(`${method}: ${JSON.stringify(reply.error)}`));
          }
        } catch (error) {
          reject(error);
        }
      }
    });
    socket.on("error", reject);
    socket.on("close", () =>
      reject(new Error(`${method}: connection closed before reply`)),
    );
  });
}

const launchpadSocket = path.join(
  os.homedir(),
  "Library/Application Support/vphone-launchpad/control.sock",
);

async function guests() {
  if (!fs.existsSync(launchpadSocket)) {
    return [];
  }
  let stdout;
  try {
    ({ stdout } = await exec("vphone-launchpad-cli", ["vm", "list"]));
  } catch (error) {
    if (error.code === "ENOENT") {
      return [];
    }
    throw error;
  }
  return JSON.parse(stdout)
    .filter((machine) => machine.state === "running" && machine.udid)
    .map((machine) => ({
      name: machine.name,
      udid: machine.udid,
      socket: machine.controlSocket,
      virtual: true,
    }));
}

// vphone guests are built with chip ID 0xFE01, which their UDID starts with
// (vphone-cli 8c3630c7, Research/Guest/xcode_install_signature_gate.md).
const guestUDID = /^0000FE01-/;

const reachability = tr(
  "unlock the device and connect it with USB or the same network as this Mac",
);

const deviceLine = (device) =>
  `  ${device.name}  ${device.udid}${device.offline ? `  ${tr("(offline)")}` : ""}`;

async function findDevice(context, { required = true, filter }) {
  const given = context.options.device;
  const [listed, virtual] = await step(
    tr("looking for devices"),
    () =>
      Promise.all([devicectl(["list", "devices"], { parse: true }), guests()]),
    { transient: true },
  );
  const paired = [
    ...listed.result.devices
      .filter(
        (device) =>
          device.hardwareProperties.platform === "iOS" &&
          ["physical", undefined].includes(device.hardwareProperties.reality) &&
          !guestUDID.test(device.hardwareProperties.udid),
      )
      .map((device) => ({
        name: device.deviceProperties.name,
        udid: device.hardwareProperties.udid,
        offline: device.connectionProperties.tunnelState === "unavailable",
      })),
    ...virtual,
  ];
  const named = (device) =>
    sameName(device.udid, given) || sameName(label(device.name), label(given));
  const devices = paired.filter((device) => !device.offline);
  const matches = given ? devices.filter(named) : devices;
  if ((given || !required) && matches.length === 1) {
    return matches[0];
  }
  if (paired.length === 0) {
    throw new Error(
      tr(
        "no iOS device is paired with this Mac; connect one with USB, unlock it, and trust this Mac",
      ),
    );
  }
  const offline = given && paired.find(named);
  if (matches.length === 0 && offline) {
    throw new Error(
      tr("{device} is offline; {hint}", {
        device: offline.name,
        hint: reachability,
      }),
    );
  }
  if (devices.length === 0) {
    throw new Error(
      `${tr("every paired iOS device is offline; {hint}:", { hint: reachability })}\n${paired.map(deviceLine).join("\n")}`,
    );
  }
  const reason = !given
    ? tr("choose a device with -d NAME|UDID")
    : matches.length === 0
      ? tr("no iOS device is named {name}", { name: given })
      : tr("more than one iOS device is named {name}; choose one by UDID", {
          name: given,
        });
  const failure = new Error(
    `${tr("{reason}; paired iOS devices:", { reason })}\n${paired.map(deviceLine).join("\n")}`,
  );
  if (matches.length === 0) {
    throw failure;
  }
  const choices =
    filter !== undefined && interactive(context)
      ? await filter(matches)
      : matches;
  return choose(
    context,
    tr("Choose a device"),
    choices.map((device) => ({
      name: `${device.name}  ${device.udid}`,
      short: device.name,
      value: device,
    })),
    failure,
  );
}

export const apps = async (device) =>
  device.virtual
    ? guestApps(device)
    : (
        await devicectl(["device", "info", "apps", "--device", device.udid], {
          parse: true,
        })
      ).result.apps;

export const lockedError = /BSErrorCodeDescription = Locked|device is locked/;

export const installApp = async (device, app, id) =>
  device.virtual
    ? guest(device, ["install", app, id])
    : devicectl(["device", "install", "app", "--device", device.udid, app]);

export const uninstallApp = async (device, id) =>
  device.virtual
    ? callGuest(device, "apps.uninstall", { bundle_id: id, force: true })
    : devicectl(["device", "uninstall", "app", "--device", device.udid, id]);

export const launchApp = async (device, id, args = []) =>
  device.virtual
    ? launchOnGuest(device, id)
    : devicectl([
        "device",
        "process",
        "launch",
        "--terminate-existing",
        "--device",
        device.udid,
        id,
        ...(args.length > 0 ? ["--", ...args] : []),
      ]);

export async function terminateApp(device, id) {
  if (device.virtual) {
    await callGuest(device, "apps.terminate", { bundle_id: id });
    return;
  }
  const [listed, { result }] = await Promise.all([
    apps(device),
    devicectl(["device", "info", "processes", "--device", device.udid], {
      parse: true,
    }),
  ]);
  const { url } = listed.find((app) => app.bundleIdentifier === id);
  const running = result.runningProcesses.find((process) =>
    process.executable?.startsWith(url),
  );
  if (running !== undefined) {
    await devicectl([
      "device",
      "process",
      "terminate",
      "--device",
      device.udid,
      "--pid",
      String(running.processIdentifier),
    ]);
  }
}

async function guestApps(device) {
  const { apps: listed } = await callGuest(device, "apps.list");
  return listed.map((app) => ({
    bundleIdentifier: app.bundle_id,
    version: app.version,
    bundleVersion: app.build,
  }));
}

async function launchOnGuest(device, id) {
  const until = Date.now() + 30_000;
  while (true) {
    try {
      await callGuest(device, "screen.unlock");
      await callGuest(device, "apps.launch", { bundle_id: id });
      return;
    } catch (error) {
      if (Date.now() > until) {
        throw error;
      }
    }
    await new Promise((resolve) => setTimeout(resolve, 1_000));
  }
}

export const configureGuest = (device, id, name) =>
  guest(device, ["configure", id, name]);

export async function coreDevice(context, options = {}) {
  const device = await findDevice(context, options);
  if (device.virtual) {
    await step(
      tr("pairing with {device}", { device: device.name }),
      () => guest(device, ["pair"]),
      { transient: true },
    );
  }
  return device;
}

async function guestContainer(device, id) {
  const { data_path: root } = await callGuest(device, "apps.data_dir", {
    bundle_id: id,
  });
  return root;
}

const containerOptions = (id) => [
  "--domain-type",
  "appDataContainer",
  "--domain-identifier",
  id,
];

export async function containerHasFile(device, id, file) {
  if (device.virtual) {
    const root = await guestContainer(device, id);
    const directory = path.posix.join(root, path.posix.dirname(file));
    const { entries } = await callGuest(device, "files.list", {
      path: directory,
    }).catch(() => ({ entries: [] }));
    return entries.some((it) => it.name === path.posix.basename(file));
  }
  const { result } = await devicectl(
    [
      "device",
      "info",
      "files",
      "--device",
      device.udid,
      ...containerOptions(id),
    ],
    { parse: true },
  );
  return result.files.some((it) => it.relativePath === file);
}

export async function pullFromContainer(device, id, source, destination) {
  if (device.virtual) {
    const root = await guestContainer(device, id);
    const { content } = await callGuest(device, "files.read", {
      path: path.posix.join(root, source),
      binary: true,
    });
    fs.writeFileSync(destination, Buffer.from(content, "base64"));
    return;
  }
  await devicectl([
    "device",
    "copy",
    "from",
    "--device",
    device.udid,
    "--source",
    source,
    "--destination",
    destination,
    ...containerOptions(id),
  ]);
}

const containerOwner = "501:501";

export async function pushToContainer(device, id, source, destination) {
  if (device.virtual) {
    const root = await guestContainer(device, id);
    const target = path.posix.join(root, destination);
    const directory = path.posix.dirname(target);
    await callGuest(device, "files.mkdir", { path: directory });
    await callGuest(device, "files.chown", {
      path: directory,
      owner: containerOwner,
    });
    await callGuest(device, "files.write", {
      path: target,
      content: fs.readFileSync(source).toString("base64"),
      encoding: "base64",
    });
    await callGuest(device, "files.chown", {
      path: target,
      owner: containerOwner,
    });
    return;
  }
  // dtfileserviced of the CoreDevice 477 Developer Disk Image creates the
  // missing parents of a copied file as root, but gives every directory of a
  // copied tree to mobile, so the file is copied inside a tree rooted at a
  // top-level directory of the container.
  const [root, ...rest] = destination.split("/");
  const work = fs.mkdtempSync(path.join(os.tmpdir(), "swipt-container-"));
  try {
    const tree = path.join(work, root);
    const target = path.join(tree, ...rest);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.copyFileSync(source, target);
    await devicectl([
      "device",
      "copy",
      "to",
      "--device",
      device.udid,
      "--source",
      tree,
      "--destination",
      root,
      ...containerOptions(id),
    ]);
  } finally {
    fs.rmSync(work, { recursive: true, force: true });
  }
}

export const provisioningProfiles = async (udid) =>
  (
    await devicectl(
      ["device", "profile", "list", "--type", "provisioning", "--device", udid],
      { parse: true },
    )
  ).result.provisioningProfiles;
