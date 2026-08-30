import json, subprocess, collections, sys

nix = sys.argv[1]
start = sys.argv[2]
seen = set()

def walk(drv):
    if drv in seen:
        return
    seen.add(drv)
    r = subprocess.run([nix, "show-derivation", drv], capture_output=True, text=True)
    if r.returncode != 0:
        print("FAILED:", drv, r.stderr[:200], file=sys.stderr)
        return
    d = json.loads(r.stdout)
    info = list(d["derivations"].values())[0]
    drvs = info.get("inputs", {}).get("drvs", {})
    for dep in drvs:
        walk("/nix/store/" + dep)

walk(start)
kinds = collections.Counter()
tu_list = []
batch_list = []
for p in seen:
    name = p.split("/")[-1]
    if "tu-" in name:
        kinds["tu"] += 1
        tu_list.append(name)
    elif "ar-" in name:
        kinds["ar"] += 1
    elif "batch-" in name:
        kinds["batch"] += 1
        batch_list.append(name)
    elif "-bin-" in name or name.startswith("bin-"):
        kinds["bin"] += 1
    else:
        kinds["other"] += 1

print("total drvs in graph:", len(seen))
print(dict(kinds))
print()
print("tu drvs:", sorted(tu_list))
print("batch drvs:", sorted(batch_list))
