# Read a single worker report with -s. The trusted collector supplies the
# eligible open issue IDs and kinds in --slurpfile catalog.
def object_keys($expected): type == "object" and keys == ($expected | sort);
def issue_number: type == "number" and . > 0 and . <= 2147483647 and floor == .;
def catalog_record:
  object_keys(["number", "kind"]) and (.number | issue_number)
  and (.kind == "bug" or .kind == "feature");
def valid_group($kinds):
  object_keys(["members", "confidence", "reason"])
  and (.members | type == "array" and length >= 2 and all(.[]; issue_number))
  and (.confidence == "high" or .confidence == "medium" or .confidence == "low")
  and ((.reason == "same-symptom" and all(.members[]; $kinds[tostring] == "bug"))
    or (.reason == "same-request" and all(.members[]; $kinds[tostring] == "feature")));

if length != 1 or ($catalog | length) != 1 then error("invalid cluster report") else .[0] end
| $catalog[0] as $records
| if ($records | type == "array" and all(.[]; catalog_record))
  then . else error("invalid cluster catalog") end
| ($records | map(.number) | sort) as $ids
| ($records | map({key: (.number | tostring), value: .kind}) | from_entries) as $kinds
| if object_keys(["clusters", "singletons"])
  and (.singletons | type == "array" and all(.[]; issue_number))
  and (.clusters | type == "array" and all(.[]; valid_group($kinds)))
  and ($ids == ($ids | unique))
  and (([.clusters[].members[], .singletons[]] | sort) == $ids)
  then {clusters: [.clusters[] | {members: (.members | map(floor) | sort), confidence, reason}],
        singletons: (.singletons | map(floor) | sort)}
  else error("invalid cluster report") end
