#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 <executor-image> <absolute-bundle-root> [bundle-version]" >&2
  exit 2
}

[[ $# -ge 2 && $# -le 3 ]] || usage

executor_image=$1
bundle_root=$2
configured_version=${3:-}

case ${bundle_root} in
  /*) ;;
  *) echo "bundle root must be an absolute path" >&2; exit 2 ;;
esac
case ${bundle_root} in
  /|/home|/var|/backup) echo "refusing broad bundle root: ${bundle_root}" >&2; exit 2 ;;
esac

docker image inspect "${executor_image}" >/dev/null
mkdir -p "${bundle_root}/releases"

container_name="pie-kroot-common-export-${$}-${RANDOM}"
stage_dir=$(mktemp -d "${bundle_root}/.stage.XXXXXX")
cleanup() {
  docker rm -f "${container_name}" >/dev/null 2>&1 || true
  case ${stage_dir} in
    "${bundle_root}"/.stage.*) rm -rf -- "${stage_dir}" ;;
  esac
}
trap cleanup EXIT

docker create --name "${container_name}" --entrypoint /bin/true "${executor_image}" >/dev/null
docker cp "${container_name}:/usr/local/share/kroot-common/." "${stage_dir}/"

for required in \
  "${stage_dir}/.claude/skills" \
  "${stage_dir}/.claude/agents"
do
  [[ -d ${required} ]] || { echo "executor image is missing ${required#${stage_dir}/}" >&2; exit 1; }
done
if find "${stage_dir}/.claude/skills" "${stage_dir}/.claude/agents" -type l -print -quit | grep -q .; then
  echo "Kroot common bundle must not contain symlinks" >&2
  exit 1
fi
skill_file_count=$(find "${stage_dir}/.claude/skills" -type f | wc -l | tr -d ' ')
agent_file_count=$(find "${stage_dir}/.claude/agents" -type f | wc -l | tr -d ' ')
file_count=$((skill_file_count + agent_file_count))
[[ ${skill_file_count} -gt 0 ]] || { echo "Kroot common bundle contains no skill files" >&2; exit 1; }
[[ ${agent_file_count} -gt 0 ]] || { echo "Kroot common bundle contains no agent files" >&2; exit 1; }

digest=$(
  (
    cd "${stage_dir}"
    while IFS= read -r -d '' path; do
      if stat -c '%a' "${path}" >/dev/null 2>&1; then
        mode=$(stat -c '%a' "${path}")
      else
        mode=$(stat -f '%Lp' "${path}")
      fi
      if [[ -d ${path} ]]; then
        printf 'D\0%s\0%s\0' "${path}" "${mode}"
      else
        content_digest=$(sha256sum "${path}" | awk '{print $1}')
        printf 'F\0%s\0%s\0%s\0' "${path}" "${mode}" "${content_digest}"
      fi
    done < <(find .claude/skills .claude/agents \( -type d -o -type f \) -print0 | sort -z)
  ) | sha256sum | awk '{print $1}'
)
if [[ -z ${configured_version} ]]; then
  configured_version=$(docker image inspect --format '{{index .Config.Labels "ai.pielab.kroot-adk-revision"}}' "${executor_image}")
fi
[[ -n ${configured_version} && ${configured_version} != '<no value>' ]] || configured_version=unknown
safe_version=$(printf '%s' "${configured_version}" | tr -cs 'A-Za-z0-9._-' '-')
safe_version=${safe_version#-}
safe_version=${safe_version%-}
[[ -n ${safe_version} ]] || safe_version=unknown

release_name="${safe_version}-${digest:0:12}"
release_dir="${bundle_root}/releases/${release_name}"
if [[ -e ${release_dir} ]]; then
  [[ -d ${release_dir} ]] || { echo "release path is not a directory: ${release_dir}" >&2; exit 1; }
else
  mv -- "${stage_dir}" "${release_dir}"
  stage_dir="${bundle_root}/.stage.consumed"
fi

next_link="${bundle_root}/.current-${$}-${RANDOM}"
ln -s "releases/${release_name}" "${next_link}"
if ! mv -Tf -- "${next_link}" "${bundle_root}/current" 2>/dev/null; then
  # BSD/macOS mv has no -T; -h gives the same no-dereference behavior for a
  # destination symlink. The production Linux path uses the first branch.
  mv -hf -- "${next_link}" "${bundle_root}/current"
fi

printf 'Kroot common bundle prepared\n'
printf '  image: %s\n' "${executor_image}"
printf '  version: %s\n' "${configured_version}"
printf '  digest: sha256:%s\n' "${digest}"
printf '  files: %s\n' "${file_count}"
printf '  current: %s\n' "${release_dir}"
