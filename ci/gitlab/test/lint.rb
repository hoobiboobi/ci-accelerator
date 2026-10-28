# The component as GitLab itself reads it: GitLab's CI lint (with the
# pipeline simulation) on pipelines that include the component, as users do,
# with every kind of input, and with inputs GitLab must refuse. Asserts the
# job GitLab builds from it (name, stage, tags, image, variables, script,
# artifacts), not only that the file is valid.
#
#   ruby ci/gitlab/test/lint.rb <api base> <token file> <gitlab url as itself> <version> <component file>
#
# Run by ci/gitlab/test/gitlab.sh against a GitLab it booted. The component is
# served by that GitLab (a public project holding the file) unless
# COMPONENT_URL names where to fetch it (the release: its tag on GitHub).
require "base64"
require "digest"
require "json"
require "net/http"
require "uri"
require "yaml"

API, TOKEN_FILE, SELF, VERSION, COMPONENT = ARGV
abort "usage: lint.rb <api base> <token file> <gitlab url> <version> <component file>" unless COMPONENT

def api(method, path, body = nil)
  uri = URI("#{API}/api/v4#{path}")
  req = Net::HTTP.const_get(method.capitalize).new(uri)
  req["PRIVATE-TOKEN"] = File.read(TOKEN_FILE).strip
  if body
    req["Content-Type"] = "application/json"
    req.body = JSON.generate(body)
  end
  res = Net::HTTP.start(uri.host, uri.port, read_timeout: 180) { |h| h.request(req) }
  raise "#{method} #{path}: #{res.code} #{res.body[0, 400]}" unless res.code.start_with?("2")
  JSON.parse(res.body)
end

def integrity(bytes)
  "sha256-#{Base64.strict_encode64(Digest::SHA256.digest(bytes))}"
end

major, minor = VERSION.split(".").map(&:to_i)
DESCRIPTION_KEY = major > 18 || (major == 18 && minor >= 10) # spec:description since 18.10

text = File.binread(COMPONENT)
spec, job_doc = YAML.load_stream(text)
inputs = spec.fetch("spec").fetch("inputs")
default_image = inputs.fetch("buildkit-image").fetch("default")
script = job_doc.values.first.fetch("script")

# The component and two copies with spec:description, one too long: a
# public project of this GitLab serves them (GitLab may fetch its own URL).
component = api(:post, "/projects", { name: "component", visibility: "public", initialize_with_readme: true })
with_description = lambda do |n|
  text.sub(/^spec:\n/, "spec:\n  description: #{"d" * n}\n")
end
files = { "build.yml" => text, "described.yml" => with_description.(200), "described-long.yml" => with_description.(257) }
api(:post, "/projects/#{component["id"]}/repository/commits", {
  branch: "main", commit_message: "the component",
  actions: files.map { |path, body| { action: "create", file_path: path, content: Base64.strict_encode64(body), encoding: "base64" } },
})
app = api(:post, "/projects", { name: "app", initialize_with_readme: true })
served = ->(file) { "#{SELF}/root/component/-/raw/main/#{file}" }
URL = ENV["COMPONENT_URL"] || served.("build.yml")
INTEGRITY = integrity(text)
puts "GitLab #{VERSION}: the component from #{URL} (#{INTEGRITY})"

$pass = 0
$fail = 0
def ok(name)
  $pass += 1
  puts "  ok    #{name}"
end

def bad(name, why)
  $fail += 1
  puts "  FAIL  #{name}\n        #{why.to_s.gsub("\n", "\n        ")}"
end

# lint <content>: GitLab's answer for a pipeline with that content, linted
# and simulated as a push to main.
$app_id = app["id"]
def lint(content)
  api(:post, "/projects/#{$app_id}/ci/lint", { content: content, dry_run: true, include_jobs: true })
end

def include_yaml(inputs, url: URL, integrity: INTEGRITY, extra: "")
  inc = { "remote" => url }
  inc["integrity"] = integrity if integrity
  inc["inputs"] = inputs if inputs
  extra + { "include" => [inc] }.to_yaml.sub(/\A---\n/, "")
end

# valid <name> <content> { |merged, jobs| ... }: GitLab accepts it; the block
# returns nil when the result is right, or what is wrong.
def valid(name, content)
  r = lint(content)
  return bad(name, "refused: #{r["errors"].join("; ")}") unless r["valid"]
  merged = YAML.safe_load(r["merged_yaml"])
  why = yield(merged, r["jobs"] || [])
  why ? bad(name, why) : ok(name)
rescue => e
  bad(name, e)
end

# refused <name> <content> <pattern>: GitLab refuses it with that message.
def refused(name, content, pattern)
  r = lint(content)
  return bad(name, "accepted") if r["valid"]
  msg = r["errors"].join("; ")
  msg =~ pattern ? ok("#{name}: #{msg[0, 160]}") : bad(name, "refused, but with: #{msg}")
rescue => e
  bad(name, e)
end

def expect(got, want, what)
  got == want ? nil : "#{what}: #{got.inspect}, want #{want.inspect}"
end

# The variables as GitLab keeps them: quoted inputs may become booleans or
# numbers after the interpolation; a job sees their text either way.
def vars(job)
  job.fetch("variables").transform_values(&:to_s)
end

# The README's include: the runner's tags, everything else by default.
valid("the README's include builds one job as the component defines it", include_yaml({ "runner-tags" => ["rc-check"] })) do |m, jobs|
  j = m["build-image"] or next "no job build-image in #{m.keys}"
  v = vars(j)
  expect(j["stage"], "build", "stage") ||
    expect(j["tags"], ["rc-check"], "tags") ||
    expect(j["image"], { "name" => default_image, "entrypoint" => [""] }, "image") ||
    expect(j["interruptible"], true, "interruptible") ||
    expect(j["script"], script, "script (the inputs must never reach it)") ||
    expect(Array(j.dig("artifacts", "reports", "dotenv")), ["build.env"], "dotenv report") ||
    expect(j.dig("artifacts", "when"), "always", "artifacts when") ||
    expect(v.values_at("CIA_CONTEXT", "CIA_DOCKERFILE", "CIA_PUSH", "CIA_REGISTRY_AUTH", "CIA_LABELS", "CIA_PROVENANCE", "CIA_TIMEOUT_MINUTES", "CIA_IMAGES"),
           [".", "", "true", "job-token", "true", "false", "60", "$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA"], "variables") ||
    expect(jobs.map { |x| [x["name"], x["stage"], x["tag_list"]] }, [["build-image", "build", ["rc-check"]]], "simulated pipeline")
end

valid("without inputs: no tags, the defaults", include_yaml(nil)) do |m, _|
  j = m["build-image"] or next "no job build-image"
  expect(Array(j["tags"]), [], "tags") || expect(vars(j)["CIA_IMAGES"], "$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA", "images")
end

every = {
  "job-name" => "build api", "stage" => "images", "runner-tags" => %w[a b], "context" => "app",
  "dockerfile" => "app/Containerfile", "target" => "runtime", "platform" => "linux/amd64,linux/arm64",
  "build-args" => "NODE_VERSION=22\nGREETING=hello world\n", "secrets" => "npmrc=NPM_TOKEN",
  "images" => "$CI_REGISTRY_IMAGE/api:$CI_COMMIT_SHORT_SHA\n$CI_REGISTRY_IMAGE/api:latest\n",
  "push" => false, "registry-auth" => "none", "labels" => false, "provenance" => true,
  "timeout-minutes" => 5, "buildctl-args" => "--no-cache\n", "buildkit-image" => "moby/buildkit:v0.34.1",
}
valid("every input reaches the job as a variable, never the script", include_yaml(every, extra: "stages: [images]\n")) do |m, jobs|
  j = m["build api"] or next "no job 'build api' in #{m.keys}"
  v = vars(j)
  want = {
    "CIA_CONTEXT" => "app", "CIA_DOCKERFILE" => "app/Containerfile", "CIA_TARGET" => "runtime",
    "CIA_PLATFORM" => "linux/amd64,linux/arm64", "CIA_BUILD_ARGS" => "NODE_VERSION=22\nGREETING=hello world\n",
    "CIA_SECRETS" => "npmrc=NPM_TOKEN", "CIA_IMAGES" => "$CI_REGISTRY_IMAGE/api:$CI_COMMIT_SHORT_SHA\n$CI_REGISTRY_IMAGE/api:latest\n",
    "CIA_PUSH" => "false", "CIA_REGISTRY_AUTH" => "none", "CIA_LABELS" => "false", "CIA_PROVENANCE" => "true",
    "CIA_TIMEOUT_MINUTES" => "5", "CIA_BUILDCTL_ARGS" => "--no-cache\n",
  }
  expect(j["stage"], "images", "stage") || expect(j["tags"], %w[a b], "tags") ||
    expect(j.dig("image", "name"), "moby/buildkit:v0.34.1", "image") ||
    expect(v.slice(*want.keys), want, "variables") || expect(j["script"], script, "script") ||
    expect(jobs.map { |x| [x["name"], x["tag_list"]] }, [["build api", %w[a b]]], "simulated pipeline")
end

two = "include:\n" + %w[api web].map { |n|
  "  - remote: #{URL}\n    integrity: #{INTEGRITY}\n    inputs:\n      job-name: build-#{n}\n      context: #{n}\n"
}.join
valid("two includes build two images", two) do |m, jobs|
  expect(jobs.map { |x| x["name"] }.sort, %w[build-api build-web], "jobs") ||
    expect(%w[build-api build-web].map { |n| vars(m[n])["CIA_CONTEXT"] }, %w[api web], "contexts")
end

refused("an input the component does not have", include_yaml({ "runner-tag" => ["x"] }), /runner-tag/)
refused("a job name the regex refuses", include_yaml({ "job-name" => "-x" }), /job-name.*(RegEx|regex)/i)
refused("a registry-auth outside its options", include_yaml({ "registry-auth" => "token" }), /registry-auth.*(allowed options|not in the list)/i)
refused("push that is not a boolean", include_yaml({ "push" => "yes" }), /push.*boolean/i)
refused("timeout-minutes that is not a number", include_yaml({ "timeout-minutes" => "soon" }), /timeout-minutes.*number/i)
refused("runner-tags that is not an array", include_yaml({ "runner-tags" => "a" }), /runner-tags.*array/i)
refused("another file under the published integrity", include_yaml(nil, url: served.("described.yml")), /integrity/i)

# spec:description: from 18.10 on, at most 256 characters (0.3.1 had 257);
# before 18.10 not a key at all, which is why the component has none.
long = include_yaml(nil, url: served.("described-long.yml"), integrity: integrity(files["described-long.yml"]))
short = include_yaml(nil, url: served.("described.yml"), integrity: integrity(files["described.yml"]))
if DESCRIPTION_KEY
  refused("a spec:description of 257 characters (0.3.1's mistake)", long, /description is too long \(maximum is 256 characters\)/)
  valid("a spec:description of 200 characters", short) { |m, _| m["build-image"] ? nil : "no job" }
else
  refused("spec:description before 18.10, any length", short, /unknown keys?:? .*description|description/i)
end

puts "GitLab #{VERSION}: #{$pass} passed, #{$fail} failed"
exit($fail.zero? ? 0 : 1)
