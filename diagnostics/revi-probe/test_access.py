import unittest

from access import Grant, active_grants


class AccessContractTests(unittest.TestCase):
    def test_only_the_authenticated_tenant_receives_reports(self):
        records = [Grant("other-report", "other-tenant", 200), Grant("mine", "tenant-a", 200)]
        self.assertEqual(active_grants(records, "tenant-a", 100), ["mine"])

    def test_expiry_is_an_exclusive_upper_bound(self):
        records = [Grant("expired", "tenant-a", 99), Grant("boundary", "tenant-a", 100), Grant("live", "tenant-a", 101)]
        self.assertEqual(active_grants(records, "tenant-a", 100), ["live"])

    def test_order_duplicates_and_input_are_preserved(self):
        records = [Grant("z", "tenant-a", 200), Grant("a", "tenant-a", 200), Grant("z", "tenant-a", 300)]
        original = list(records)
        self.assertEqual(active_grants(records, "tenant-a", 100), ["z", "a", "z"])
        self.assertEqual(records, original)

    def test_empty_input(self):
        self.assertEqual(active_grants([], "tenant-a", 100), [])


if __name__ == "__main__":
    unittest.main()
