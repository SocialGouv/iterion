cask "iterion-desktop" do
  version "3.10.4"
  sha256 "b31c4e9c51acc78aa8a32412afc983d210c94879a712a55f6945b2ce146243de"

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
