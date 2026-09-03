cask "iterion-desktop" do
  version "3.94.3"
  sha256 "f0bbc8f2e44f9fe02558a2de18e48e417681b85e54c4461c17d022f2e7a4aa65"

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
