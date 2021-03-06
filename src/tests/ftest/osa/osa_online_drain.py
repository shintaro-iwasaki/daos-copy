#!/usr/bin/python
"""
  (C) Copyright 2020-2021 Intel Corporation.

  SPDX-License-Identifier: BSD-2-Clause-Patent
"""
import time
import random
import threading

from itertools import product
from test_utils_pool import TestPool
from write_host_file import write_host_file
from osa_utils import OSAUtils
from apricot import skipForTicket
import queue


class OSAOnlineDrain(OSAUtils):
    # pylint: disable=too-many-ancestors
    """
    Test Class Description: This test runs
    daos_server Online Drain test cases.

    :avocado: recursive
    """
    def setUp(self):
        """Set up for test case."""
        super().setUp()
        self.dmg_command = self.get_dmg_command()
        self.ior_flags = self.params.get("ior_flags", '/run/ior/iorflags/*')
        self.ior_apis = self.params.get("ior_api", '/run/ior/iorflags/*')
        self.ior_test_sequence = self.params.get(
            "ior_test_sequence", '/run/ior/iorflags/*')
        self.ior_dfs_oclass = self.params.get(
            "obj_class", '/run/ior/iorflags/*')
        # Recreate the client hostfile without slots defined
        self.hostfile_clients = write_host_file(
            self.hostlist_clients, self.workdir, None)
        self.pool = None
        self.out_queue = queue.Queue()

    def run_online_drain_test(self, num_pool):
        """Run the Online drain without data.
            Args:
             int : total pools to create for testing purposes.
        """
        num_jobs = self.params.get("no_parallel_job", '/run/ior/*')
        # Create a pool
        pool = {}
        pool_uuid = []
        target_list = []
        drain_servers = (len(self.hostlist_servers) * 2) - 1

        # Exclude target : random two targets  (target idx : 0-7)
        n = random.randint(0, 6)
        target_list.append(n)
        target_list.append(n+1)
        t_string = "{},{}".format(target_list[0], target_list[1])

        # Drain one of the ranks (or server)
        rank = random.randint(1, drain_servers)

        for val in range(0, num_pool):
            pool[val] = TestPool(self.context, self.get_dmg_command())
            pool[val].get_params(self)
            # Split total SCM and NVME size for creating multiple pools.
            pool[val].scm_size.value = int(pool[val].scm_size.value /
                                           num_pool)
            pool[val].nvme_size.value = int(pool[val].nvme_size.value /
                                            num_pool)
            pool[val].create()
            pool_uuid.append(pool[val].uuid)

        # Drain the pool_uuid, rank and targets
        for val in range(0, num_pool):
            threads = []
            for oclass, api, test, flags in product(self.ior_dfs_oclass,
                                                    self.ior_apis,
                                                    self.ior_test_sequence,
                                                    self.ior_flags):
                for thrd in range(0, num_jobs):
                    # Add a thread for these IOR arguments
                    threads.append(threading.Thread(target=self.ior_thread,
                                                    kwargs={"pool": pool[val],
                                                            "oclass": oclass,
                                                            "api": api,
                                                            "test": test,
                                                            "flags": flags,
                                                            "results":
                                                            self.out_queue}))
                # Launch the IOR threads
                for thrd in threads:
                    self.log.info("Thread : %s", thrd)
                    thrd.start()
                    time.sleep(1)
            self.pool = pool[val]
            self.pool.display_pool_daos_space("Pool space: Beginning")
            pver_begin = self.get_pool_version()
            self.log.info("Pool Version at the beginning %s", pver_begin)
            output = self.dmg_command.pool_drain(self.pool.uuid,
                                                 rank, t_string)
            self.log.info(output)
            self.is_rebuild_done(3)
            self.assert_on_rebuild_failure()

            pver_drain = self.get_pool_version()
            self.log.info("Pool Version after drain %s", pver_drain)
            # Check pool version incremented after pool exclude
            self.assertTrue(pver_drain > pver_begin,
                            "Pool Version Error:  After drain")
            # Wait to finish the threads
            for thrd in threads:
                thrd.join(timeout=20)

        for val in range(0, num_pool):
            display_string = "Pool{} space at the End".format(val)
            self.pool = pool[val]
            self.pool.display_pool_daos_space(display_string)

    @skipForTicket("DAOS-6664")
    def test_osa_online_drain(self):
        """Test ID: DAOS-4750
        Test Description: Validate Online drain

        :avocado: tags=all,pr,daily_regression
        :avocado: tags=hw,medium,ib2
        :avocado: tags=osa,checksum,DAOS_5610
        :avocado: tags=osa_drain,online_drain
        """
        # Perform drain testing with 1 to 2 pools
        for pool_num in range(1, 3):
            self.run_online_drain_test(pool_num)
