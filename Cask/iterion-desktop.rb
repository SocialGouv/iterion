cask "iterion-desktop" do
  version "3.70.1"
  sha256 "5938d1bfd39a69e39352f57f2768aa46dade028cf61ebff656134576d22b5fd5"

  url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-desktop-darwin-universal.zip"
  name "Iterion Desktop"
  desc "Workflow orchestration engine — desktop app"
  homepage "https://github.com/SocialGouv/iterion"

  livecheck do
    url :url
    strategy :github_latest
  end

  app "Iterion.app"

  zap trash: [
    "~/Library/Application Support/Iterion",
    "~/Library/Caches/Iterion",
    "~/Library/Logs/Iterion",
    "~/Library/Preferences/com.iterion.Iterion.plist",
    "~/Library/Saved Application State/com.iterion.Iterion.savedState",
  ]
end
